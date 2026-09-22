package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testRefreshApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := NewApp()
	writeTestFile(t, a.manager.currentPath, "work\n")
	writeTestFile(t, a.manager.authPath, testAuth("work", "old"))
	writeTestFile(t, filepath.Join(a.manager.accountsDir, "work.json"), testAuth("work", "old"))
	writeTestFile(t, filepath.Join(a.manager.accountsDir, "main.json"), testAuth("main", "main-token"))
	return a
}

func testAuth(account, token string) string {
	return fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":%q,"refresh_token":%q}}`, account, token, token+"-refresh")
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := writeTextAtomic(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeCodex(t *testing.T, script string) {
	t.Helper()
	root := t.TempDir()
	// Match the native binary layout recognized by codexCommandPath.
	for _, path := range []string{
		filepath.Join(root, "bin", "codex"),
		filepath.Join(root, "vendor", codexTargetTriple(), "codex", "codex"),
	} {
		writeTestFile(t, path, "#!/bin/sh\nset -eu\n"+script)
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestUsageRefreshPicksUpActiveCredentialRenewal(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual=%t", manual), func(t *testing.T) {
			a := testRefreshApp(t)
			fakeCodex(t, "exit 1\n")
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"rate_limit":{"primary_window":{"used_percent":54,"limit_window_seconds":604800,"reset_at":1790428295}}}`
				status := http.StatusOK
				if r.Header.Get("Authorization") == "Bearer old" {
					status = http.StatusUnauthorized
					body = `{"error":{"code":"token_expired"}}`
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})

			if err := a.loadAccountsFromDisk(); err != nil {
				t.Fatal(err)
			}
			// Codex renews auth while Ember is already running.
			writeTestFile(t, a.manager.authPath, testAuth("work", "renewed"))
			if manual {
				a.HandleMenuAction(TagRefresh)
			} else if err := a.refreshAccounts(false); err != nil {
				t.Fatal(err)
			}
			entries := buildMenuEntries(a.accounts, a.activeName, a.lastError)
			found := false
			for _, entry := range entries {
				if entry.Title == "work    Weekly 54% used" && entry.Checked {
					found = true
				}
			}
			if !found {
				t.Fatalf("refreshed menu is missing active work usage: %+v", entries)
			}
			assertFileContents(t, filepath.Join(a.manager.accountsDir, "work.json"), testAuth("work", "renewed"))
			assertFileContents(t, filepath.Join(a.manager.accountsDir, "main.json"), testAuth("main", "main-token"))
			if a.state.Accounts["work"].Usage.WeeklyReset.IsZero() {
				t.Fatal("weekly reset is missing")
			}
			// A later refresh must observe another renewal as well.
			writeTestFile(t, a.manager.authPath, testAuth("work", "renewed-again"))
			a.state.Accounts["work"].LastUsageFetchedAt = time.Now().Add(-usageRefreshWindow)
			if err := a.refreshAccounts(false); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, filepath.Join(a.manager.accountsDir, "work.json"), testAuth("work", "renewed-again"))
		})
	}
}

func TestUsageProbePreservesRenewal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		account    string
		requestErr bool
	}{
		{name: "active", account: "work"},
		{name: "inactive", account: "main"},
		{name: "active request fails after renewal", account: "work", requestErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testRefreshApp(t)
			m := a.manager
			outerHome := t.TempDir()
			t.Setenv("CODEX_HOME", outerHome)
			writeTestFile(t, filepath.Join(outerHome, "auth.json"), "outer-auth")
			renewed := testAuth(tc.account, "probe-renewed")
			renewalPath := filepath.Join(t.TempDir(), "renewed.json")
			writeTestFile(t, renewalPath, renewed)
			t.Setenv("EMBER_TEST_RENEWAL", renewalPath)
			script := `test "$CODEX_HOME" = "$HOME/.codex"
cp "$EMBER_TEST_RENEWAL" "$CODEX_HOME/auth.json"
mkdir -p "$CODEX_HOME/sessions"
printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":54,"window_minutes":10080,"resets_at":1790428295}}}}' > "$CODEX_HOME/sessions/probe.jsonl"
`
			if tc.requestErr {
				script += "echo 'request failed after renewal' >&2\nexit 1\n"
			}
			fakeCodex(t, script)
			account, err := m.readAccount(tc.account, filepath.Join(m.accountsDir, tc.account+".json"))
			if err != nil {
				t.Fatal(err)
			}
			usage, err := m.fetchUsageFromExec(account)
			if tc.requestErr {
				if err == nil || !strings.Contains(err.Error(), "request failed after renewal") {
					t.Fatalf("probe error = %v", err)
				}
			} else if err != nil || usage.WeeklyUsedPct == nil || *usage.WeeklyUsedPct != 54 {
				t.Fatalf("probe usage = %+v, error = %v", usage, err)
			}
			assertFileContents(t, account.Path, renewed)
			if tc.account == "work" {
				assertFileContents(t, m.authPath, renewed)
			} else {
				assertFileContents(t, m.authPath, testAuth("work", "old"))
			}
			assertFileContents(t, filepath.Join(outerHome, "auth.json"), "outer-auth")
			// Persistence on the next refresh, switch, or shutdown must not
			// overwrite the probe's rotated refresh token.
			if err := m.PersistCurrentAuth(); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, account.Path, renewed)
		})
	}
}

func TestUsageProbeDoesNotOverwriteChangedCredentials(t *testing.T) {
	for _, change := range []string{"active", "snapshot", "different account", "invalid", "unchanged probe"} {
		t.Run(change, func(t *testing.T) {
			a := testRefreshApp(t)
			m := a.manager
			account, err := m.readAccount("work", filepath.Join(m.accountsDir, "work.json"))
			if err != nil {
				t.Fatal(err)
			}
			original := testAuth("work", "old")
			active, snapshot := original, original
			renewed := testAuth("work", "probe-renewed")
			switch change {
			case "active":
				active = testAuth("work", "codex-renewed")
			case "snapshot":
				snapshot = testAuth("work", "externally-renewed")
			case "different account":
				renewed = testAuth("other", "probe-renewed")
			case "invalid":
				renewed = `{}`
			case "unchanged probe":
				renewed = original
				active = testAuth("work", "codex-renewed")
			}
			writeTestFile(t, m.authPath, active)
			writeTestFile(t, account.Path, snapshot)
			probePath := filepath.Join(t.TempDir(), "auth.json")
			writeTestFile(t, probePath, renewed)
			err = m.persistProbeAuth(account, []byte(original), probePath)
			if (err == nil) != (change == "unchanged probe") {
				t.Fatalf("unexpected persistence error: %v", err)
			}
			assertFileContents(t, m.authPath, active)
			assertFileContents(t, account.Path, snapshot)
		})
	}
}

func TestUsageRefreshStopsWhenCredentialSyncFails(t *testing.T) {
	a := testRefreshApp(t)
	writeTestFile(t, a.manager.currentPath, "../invalid\n")
	if err := a.refreshAccounts(true); err == nil || !strings.Contains(err.Error(), "persist current auth before usage refresh") {
		t.Fatalf("refresh error = %v", err)
	}
}
