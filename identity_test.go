package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestMenuRefreshPreservesAccountAfterExternalLogin(t *testing.T) {
	a := testRefreshApp(t)
	// The user signs in as main through Codex without changing Ember's marker.
	writeTestFile(t, a.manager.authPath, testAuth("main", "external-login"))
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":16,"window_minutes":10080}}}`))}, nil
	})
	a.HandleMenuAction(TagRefresh)
	assertFileContents(t, filepath.Join(a.manager.accountsDir, "work.json"), testAuth("work", "old"))
	assertFileContents(t, a.manager.authPath, testAuth("main", "external-login"))
	if a.lastError == "" {
		t.Fatal("menu must explain the mismatched active login")
	}
	if a.activeName != "" {
		t.Fatal("menu must not mark the wrong saved login as active")
	}
}

func TestSwitchPreservesSavedLoginAfterExternalLogin(t *testing.T) {
	a := testRefreshApp(t)
	writeTestFile(t, a.manager.authPath, testAuth("main", "external-login"))
	if err := a.manager.SwitchToAccount("main"); err == nil {
		t.Fatal("switch must stop rather than overwrite the current snapshot")
	}
	assertFileContents(t, filepath.Join(a.manager.accountsDir, "work.json"), testAuth("work", "old"))
	assertFileContents(t, a.manager.currentPath, "work\n")
}

func TestDuplicateLoginMenuNamesTheActiveAlias(t *testing.T) {
	a := testRefreshApp(t)
	writeTestFile(t, filepath.Join(a.manager.accountsDir, "main.json"), testAuth("work", "another-session"))
	if err := a.loadAccountsFromDisk(); err != nil {
		t.Fatal(err)
	}
	entries := buildMenuEntries(a.accounts, a.activeName, "")
	for _, entry := range entries {
		if entry.Title == "main    Same login as work" {
			return
		}
	}
	t.Fatalf("duplicate login is presented as a separate allowance: %+v", entries)
}

func authWithUser(account, user, token string) string {
	claims := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_user_id":%q}}`, account, user)
	id := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	return fmt.Sprintf(`{"tokens":{"account_id":%q,"id_token":%q,"access_token":%q,"refresh_token":%q}}`, account, id, token, token+"-refresh")
}

func TestPersistenceDistinguishesUsersInSameOrganization(t *testing.T) {
	a := testRefreshApp(t)
	path := filepath.Join(a.manager.accountsDir, "work.json")
	original := authWithUser("organization", "first-user", "old")
	writeTestFile(t, path, original)
	writeTestFile(t, a.manager.authPath, authWithUser("organization", "second-user", "new"))
	if err := a.manager.PersistCurrentAuth(); err == nil {
		t.Fatal("different user in same organization must not overwrite saved login")
	}
	assertFileContents(t, path, original)
}

func TestProbeRejectsDifferentUserInSameOrganization(t *testing.T) {
	a := testRefreshApp(t)
	path := filepath.Join(a.manager.accountsDir, "work.json")
	original := authWithUser("organization", "first-user", "old")
	writeTestFile(t, path, original)
	writeTestFile(t, a.manager.authPath, original)
	account, err := a.manager.readAccount("work", path)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "auth.json")
	writeTestFile(t, probe, authWithUser("organization", "second-user", "new"))
	if err := a.manager.persistProbeAuth(account, []byte(original), probe); err == nil {
		t.Fatal("warming renewal must preserve the user as well as the organization")
	}
	assertFileContents(t, path, original)
	assertFileContents(t, a.manager.authPath, original)
}

func TestDuplicateLoginRefreshReadsAllowanceOnlyOnce(t *testing.T) {
	a := testRefreshApp(t)
	writeTestFile(t, filepath.Join(a.manager.accountsDir, "main.json"), testAuth("work", "another-session"))
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	requests := 0
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Header.Get("Authorization") != "Bearer old" {
			t.Error("usage must use the active alias")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":16,"window_minutes":10080}}}`))}, nil
	})
	a.HandleMenuAction(TagRefresh)
	if requests != 1 {
		t.Fatalf("requests = %d, want one for the shared allowance", requests)
	}
	if a.state.Accounts["main"].Usage.WeeklyUsedPct != nil || a.state.Accounts["main"].Warmup.Phase != "" {
		t.Fatal("duplicate alias must not maintain a separate usage or warming state")
	}
}
