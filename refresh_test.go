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
	writeTestFile(t, a.manager.homesPath, `[{"name":"work","home":"~/.codex"},{"name":"main","home":"~/.codex_main"}]`)
	writeTestFile(t, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"), testAuth("work", "old"))
	writeTestFile(t, filepath.Join(os.Getenv("HOME"), ".codex_main", "auth.json"), testAuth("main", "main-token"))
	return a
}

func testAuth(account, token string) string {
	return fmt.Sprintf(`{"tokens":{"account_id":%q,"access_token":%q}}`, account, token)
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
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

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func TestUsageRefreshReadsEachHomeWithoutWritingCredentials(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual=%t", manual), func(t *testing.T) {
			a := testRefreshApp(t)
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			requests := map[string]string{}
			http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
				requests[r.Header.Get("OpenAI-Account-ID")] = r.Header.Get("Authorization")
				body := `{"rate_limit":{"primary_window":{"used_percent":54,"limit_window_seconds":604800,"reset_at":1790428295}}}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err := a.loadAccountsFromDisk(); err != nil {
				t.Fatal(err)
			}
			workAuth := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
			writeTestFile(t, workAuth, testAuth("work", "renewed"))
			if manual {
				a.HandleMenuAction(TagRefresh)
			} else if err := a.refreshAccounts(false); err != nil {
				t.Fatal(err)
			}
			if requests["work"] != "Bearer renewed" || requests["main"] != "Bearer main-token" {
				t.Fatalf("usage credentials = %v", requests)
			}
			for _, entry := range buildMenuEntries(a.accounts, a.lastError) {
				if entry.Enabled && entry.Tag != TagRefresh && entry.Tag != TagQuit {
					t.Fatalf("unexpected clickable entry: %+v", entry)
				}
			}
			assertFileContents(t, workAuth, testAuth("work", "renewed"))
			assertFileContents(t, filepath.Join(os.Getenv("HOME"), ".codex_main", "auth.json"), testAuth("main", "main-token"))
			if a.state.Accounts["work"].Usage.WeeklyReset.IsZero() {
				t.Fatal("weekly reset is missing")
			}
			writeTestFile(t, workAuth, testAuth("work", "renewed-again"))
			a.state.Accounts["work"].LastUsageFetchedAt = time.Now().Add(-usageRefreshWindow)
			if err := a.refreshAccounts(false); err != nil {
				t.Fatal(err)
			}
			if requests["work"] != "Bearer renewed-again" {
				t.Fatalf("next refresh reused stale credentials: %v", requests)
			}
		})
	}
}

func TestMissingHomeLoginStaysVisible(t *testing.T) {
	a := testRefreshApp(t)
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".codex_main", "auth.json")); err != nil {
		t.Fatal(err)
	}
	// The other home's direct usage read succeeds without starting Codex.
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":20,"window_minutes":10080}}}`))}, nil
	})
	if err := a.refreshAccounts(true); err != nil {
		t.Fatal(err)
	}
	if len(a.accounts) != 2 || a.state.Accounts["main"].Usage.Error == "" {
		t.Fatalf("missing login disappeared: %+v", a.accounts)
	}
	for _, entry := range buildMenuEntries(a.accounts, "") {
		if strings.Contains(entry.Title, "read main login:") {
			return
		}
	}
	t.Fatal("menu must show the login error")
}
