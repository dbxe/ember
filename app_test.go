package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseUsageFromRateLimitsUsesSevenDayPrimaryWindow(t *testing.T) {
	raw := json.RawMessage(`{
		"primary": {"used_percent": 98, "window_minutes": 10080, "resets_at": 1788748007},
		"secondary": null
	}`)

	usage, err := parseUsageFromRateLimits(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.WeeklyUsedPct == nil || *usage.WeeklyUsedPct != 98 {
		t.Fatalf("WeeklyUsedPct = %v, want 98", usage.WeeklyUsedPct)
	}
	if got := usage.WeeklyReset.Unix(); got != 1788748007 {
		t.Fatalf("WeeklyReset = %d, want 1788748007", got)
	}
}

func TestParseUsageFromRateLimitsSupportsLegacySecondaryWindow(t *testing.T) {
	raw := json.RawMessage(`{
		"primary": {"used_percent": 12, "window_minutes": 300},
		"secondary": {"used_percent": 34, "window_minutes": 10080}
	}`)

	usage, err := parseUsageFromRateLimits(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.WeeklyUsedPct == nil || *usage.WeeklyUsedPct != 34 {
		t.Fatalf("WeeklyUsedPct = %v, want 34", usage.WeeklyUsedPct)
	}
}

func TestParseUsagePayloadConvertsRemainingPercent(t *testing.T) {
	payload := map[string]any{
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"remaining_percent":    76.0,
				"limit_window_seconds": float64(7 * 24 * 60 * 60),
			},
		},
	}

	usage, err := parseUsagePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if usage.WeeklyUsedPct == nil || *usage.WeeklyUsedPct != 24 {
		t.Fatalf("WeeklyUsedPct = %v, want 24", usage.WeeklyUsedPct)
	}
}

func TestFormatWeeklyUsageIsExplicit(t *testing.T) {
	used := 98.0
	if got := formatWeeklyUsage(UsageSnapshot{WeeklyUsedPct: &used}); got != "Weekly 98% used" {
		t.Fatalf("formatWeeklyUsage() = %q", got)
	}
	if got := formatWeeklyUsage(UsageSnapshot{}); got != "Weekly --" {
		t.Fatalf("formatWeeklyUsage() = %q", got)
	}
}

func TestBuildMenuEntriesContainsOneRowAndCheckmarkPerAccount(t *testing.T) {
	mainUsed := 12.0
	workUsed := 34.0
	accounts := []AccountView{
		{
			Account: Account{Name: "main", Email: "main@example.com", Plan: "pro"},
			Active:  true,
			Cache:   AccountCache{Usage: UsageSnapshot{WeeklyUsedPct: &mainUsed}},
		},
		{
			Account: Account{Name: "work", Email: "work@example.com", Plan: "team"},
			Cache:   AccountCache{Usage: UsageSnapshot{WeeklyUsedPct: &workUsed}},
		},
	}

	entries := buildMenuEntries(accounts, "main", "")
	titleCounts := map[string]int{}
	checked := 0
	for _, entry := range entries {
		if entry.Separator {
			continue
		}
		titleCounts[entry.Title]++
		if entry.Checked {
			checked++
		}
	}

	if titleCounts["main    Weekly 12% used"] != 1 {
		t.Fatalf("main row count = %d, want 1", titleCounts["main    Weekly 12% used"])
	}
	if titleCounts["work    Weekly 34% used"] != 1 {
		t.Fatalf("work row count = %d, want 1", titleCounts["work    Weekly 34% used"])
	}
	if titleCounts["Refresh usage"] != 1 || titleCounts["Quit Ember"] != 1 {
		t.Fatalf("action counts = refresh %d, quit %d", titleCounts["Refresh usage"], titleCounts["Quit Ember"])
	}
	if checked != 1 {
		t.Fatalf("checked rows = %d, want 1", checked)
	}
}

func TestSwitchToAccountPersistsCurrentAuthAndActivatesTarget(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, ".codex")
	accountsDir := filepath.Join(codexDir, "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	mainSnapshot := filepath.Join(accountsDir, "main.json")
	workSnapshot := filepath.Join(accountsDir, "work.json")
	authPath := filepath.Join(codexDir, "auth.json")
	if err := os.WriteFile(mainSnapshot, []byte("main-old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workSnapshot, []byte("work-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte("main-refreshed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "current"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := &AccountManager{
		codexDir:    codexDir,
		accountsDir: accountsDir,
		authPath:    authPath,
		currentPath: filepath.Join(codexDir, "current"),
	}
	if err := manager.SwitchToAccount("work"); err != nil {
		t.Fatal(err)
	}

	assertFileContents(t, mainSnapshot, "main-refreshed")
	assertFileContents(t, authPath, "work-auth")
	assertFileContents(t, manager.currentPath, "work\n")
}

func TestPersistCurrentAuthBootstrapsFirstSnapshot(t *testing.T) {
	root := t.TempDir()
	codexDir := filepath.Join(root, ".codex")
	accountsDir := filepath.Join(codexDir, "accounts")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(codexDir, "auth.json")
	currentPath := filepath.Join(codexDir, "current")
	if err := os.WriteFile(authPath, []byte("current-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := &AccountManager{
		accountsDir: accountsDir,
		authPath:    authPath,
		currentPath: currentPath,
	}
	if err := manager.PersistCurrentAuth(); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(accountsDir, "main.json"), "current-auth")
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
