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

func weeklyUsage(used float64, reset time.Time) UsageSnapshot {
	return UsageSnapshot{WeeklyUsedPct: &used, WeeklyReset: reset}
}

func TestColdWeeklyUsage(t *testing.T) {
	now := time.Date(2026, 9, 27, 9, 0, 30, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		usage UsageSnapshot
		cold  bool
	}{
		{"sliding", weeklyUsage(0, now.Add(weeklyWindow)), true},
		{"minute rounding", weeklyUsage(0, now.Add(weeklyWindow).Truncate(time.Minute)), true},
		{"rounded usage is not zero", weeklyUsage(.01, now.Add(weeklyWindow)), false},
		{"countdown", weeklyUsage(0, now.Add(weeklyWindow-2*time.Minute)), false},
		{"missing reset", weeklyUsage(0, time.Time{}), false},
		{"missing usage", UsageSnapshot{WeeklyReset: now.Add(weeklyWindow)}, false},
		{"past", weeklyUsage(0, now.Add(-time.Second)), false},
		{"distant", weeklyUsage(0, now.Add(weeklyWindow+2*time.Minute)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coldWeeklyUsage(tc.usage, now); got != tc.cold {
				t.Fatalf("cold=%v want %v", got, tc.cold)
			}
		})
	}
}

func TestWarmupRequiresMovementThenFixedCountdown(t *testing.T) {
	now := time.Now()
	var w WarmupState
	if w.observe(weeklyUsage(0, now.Add(weeklyWindow)), now) {
		t.Fatal("first sample must not probe")
	}
	later := now.Add(warmupCheckDelay)
	if w.observe(weeklyUsage(0, now.Add(weeklyWindow)), later) || w.Phase != "" {
		t.Fatal("freshly warmed zero-usage account must not probe")
	}
	w = WarmupState{}
	w.observe(weeklyUsage(0, now.Add(weeklyWindow)), now)
	if !w.observe(weeklyUsage(0, later.Add(weeklyWindow)), later) {
		t.Fatal("moving reset must probe")
	}
	w.begin(later)
	w.Phase = "verifying"
	reset := later.Add(weeklyWindow)
	w.observe(weeklyUsage(0, reset), later)
	w.observe(weeklyUsage(0, reset), later.Add(30*time.Second))
	if w.Phase != "verifying" {
		t.Fatal("must wait for countdown")
	}
	w.observe(weeklyUsage(0, reset), later.Add(warmupCheckDelay))
	if w.Phase != "confirmed" {
		t.Fatalf("phase=%s", w.Phase)
	}
}

func TestUnconfirmedWarmupBacksOff(t *testing.T) {
	now := time.Now()
	var w WarmupState
	w.begin(now)
	w.Phase = "verifying"
	w.observe(weeklyUsage(0, now.Add(weeklyWindow)), now)
	later := now.Add(usageRefreshWindow)
	w.observe(weeklyUsage(0, later.Add(weeklyWindow)), later)
	if w.Phase != "retry" || w.Error == "" {
		t.Fatalf("state=%+v", w)
	}
	for i := 1; i < 40; i++ {
		at := later.Add(time.Duration(i) * time.Minute)
		if w.observe(weeklyUsage(0, at.Add(weeklyWindow)), at) {
			t.Fatal("probe during backoff")
		}
	}
	at := now.Add(2 * time.Hour)
	if w.observe(weeklyUsage(0, at.Add(weeklyWindow)), at) {
		t.Fatal("retry requires fresh evidence")
	}
	at = at.Add(warmupCheckDelay)
	if !w.observe(weeklyUsage(0, at.Add(weeklyWindow)), at) {
		t.Fatal("retry never became eligible")
	}
}

const acknowledgmentScript = `
test "$1" = exec
test "$HOME" = "$EMBER_TEST_HOME"
test "$CODEX_HOME" = "$EMBER_TEST_CODEX_HOME"
test "$(pwd -P)" != "$(cd "$CODEX_HOME" && pwd -P)"
test -z "$(ls -A)"
if test -n "${EMBER_TEST_WORKDIR:-}"; then pwd -P > "$EMBER_TEST_WORKDIR"; fi
test -z "${OPENAI_API_KEY+x}"
test -z "${CODEX_API_KEY+x}"
test -z "${OPENAI_BASE_URL+x}"
if test -n "${EMBER_TEST_PROBES:-}"; then echo probe >> "$EMBER_TEST_PROBES"; fi
model=""
while [ "$#" -gt 0 ]; do
 case "$1" in
  --model) shift; model="$1" ;;
  --output-last-message) shift; test "$(cd "$(dirname "$1")" && pwd -P)" = "$(pwd -P)"; printf 'OK\n' > "$1" ;;
 esac
 shift
done
test "$model" = gpt-6-luna
`

func TestRefreshWarmsHomeOnceAndPersistsConfirmation(t *testing.T) {
	a := testRefreshApp(t)
	now := time.Now().Truncate(time.Second)
	a.now = func() time.Time { return now }
	marker := filepath.Join(t.TempDir(), "probes")
	t.Setenv("EMBER_TEST_PROBES", marker)
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("CODEX_API_KEY", "must-not-inherit")
	t.Setenv("OPENAI_BASE_URL", "must-not-inherit")

	t.Setenv("EMBER_TEST_HOME", os.Getenv("HOME"))
	t.Setenv("EMBER_TEST_CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex_main"))
	t.Setenv("CODEX_HOME", "must-not-inherit")
	workdirMarker := filepath.Join(t.TempDir(), "workdir")
	t.Setenv("EMBER_TEST_WORKDIR", workdirMarker)
	fakeCodex(t, acknowledgmentScript)
	var fixed time.Time
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		reset := now.Add(weeklyWindow)
		used := 0
		if r.Header.Get("OpenAI-Account-ID") == "work" {
			used = 20
			reset = now.Add(24 * time.Hour)
		} else {
			if fileExists(marker) && fixed.IsZero() {
				fixed = reset
			}
			if !fixed.IsZero() {
				reset = fixed
			}
		}
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`, used, reset.Unix())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	refresh := func() {
		t.Helper()
		if err := a.refreshAccounts(true); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	if fileExists(marker) {
		t.Fatal("probe before confirming movement")
	}
	now = now.Add(warmupCheckDelay)
	refresh()
	if a.state.Accounts["main"].Warmup.Phase != "verifying" {
		t.Fatalf("missing pending verification: %+v", a.state.Accounts["main"].Warmup)
	}
	assertFileContents(t, marker, "probe\n")
	assertFileContents(t, filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"), testAuth("work", "old"))
	assertFileContents(t, filepath.Join(os.Getenv("HOME"), ".codex_main", "auth.json"), testAuth("main", "main-token"))
	workdir, err := os.ReadFile(workdirMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(workdir))); !os.IsNotExist(err) {
		t.Fatalf("probe directory was not removed: %v", err)
	}
	// A restart and repeated manual refresh must not submit another probe.
	a = NewApp()
	a.now = func() time.Time { return now }
	if err := a.loadState(); err != nil {
		t.Fatal(err)
	}
	refresh()
	now = now.Add(warmupCheckDelay)
	refresh()
	if a.state.Accounts["main"].Warmup.Phase != "confirmed" {
		t.Fatalf("warmup=%+v", a.state.Accounts["main"].Warmup)
	}
	entries := buildMenuEntries(a.accounts, "")
	found := false
	for _, e := range entries {
		if strings.Contains(e.Title, "countdown confirmed") {
			found = true
		}
	}
	if !found {
		t.Fatal("menu does not confirm warming")
	}
	now = now.Add(usageRefreshWindow)
	refresh()
	assertFileContents(t, marker, "probe\n")
}

func TestWarmProbeRequiresResponse(t *testing.T) {
	a := testRefreshApp(t)
	fakeCodex(t, "exit 0\n")
	accounts, err := a.manager.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.manager.WarmAccount(accounts[1]); err == nil || !strings.Contains(err.Error(), "no acknowledgment") {
		t.Fatalf("error=%v", err)
	}
}

func TestReadOnlyFallback(t *testing.T) {
	a := testRefreshApp(t)
	t.Setenv("EMBER_TEST_HOME", os.Getenv("HOME"))
	t.Setenv("EMBER_TEST_CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex_main"))
	t.Setenv("CODEX_HOME", "must-not-inherit")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("CODEX_API_KEY", "must-not-inherit")
	t.Setenv("OPENAI_BASE_URL", "must-not-inherit")
	fakeCodex(t, `test "$*" = "app-server --listen stdio://"
test "$HOME" = "$EMBER_TEST_HOME"
test "$CODEX_HOME" = "$EMBER_TEST_CODEX_HOME"
test "$(pwd -P)" = "$(cd "$CODEX_HOME" && pwd -P)"
test -z "${OPENAI_API_KEY+x}"
test -z "${CODEX_API_KEY+x}"
test -z "${OPENAI_BASE_URL+x}"
read -r initialize
case "$initialize" in *'"method":"initialize"'*) ;; *) exit 1 ;; esac
printf '%s\n' '{"id":1,"result":{}}'
read -r initialized
read -r request
case "$request" in *'"method":"account/rateLimits/read"'*) ;; *) exit 1 ;; esac
printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":90,"windowDurationMins":10080,"resetsAt":1790428295}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":0,"windowDurationMins":10080,"resetsAt":1790428295}}}}}'
`)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader("expired"))}, nil
	})
	accounts, err := a.manager.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.manager.FetchUsage(accounts[1])
	if err != nil || u.WeeklyUsedPct == nil || *u.WeeklyUsedPct != 0 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
	assertFileContents(t, filepath.Join(accounts[1].Home, "auth.json"), testAuth("main", "main-token"))
}

func TestInterruptedProbeAndMissingResetDoNotLoop(t *testing.T) {
	now := time.Now()
	var w WarmupState
	w.begin(now)
	later := now.Add(warmupCheckDelay)
	if w.observe(weeklyUsage(0, later.Add(weeklyWindow)), later) || w.Phase != "retry" {
		t.Fatal("interrupted probe must back off")
	}
	later = w.NextCheckAt
	w.observe(UsageSnapshot{}, later)
	if !w.NextCheckAt.After(later) {
		t.Fatal("missing reset would spin the refresh loop")
	}
}

func TestRefreshScheduleReturnsToSleep(t *testing.T) {
	a := NewApp()
	now := time.Now()
	a.now = func() time.Time { return now }
	a.state.Accounts["main"] = &AccountCache{LastUsageFetchedAt: now, Warmup: WarmupState{NextCheckAt: now.Add(warmupCheckDelay)}}
	if got := a.nextRefreshDelay(); got != warmupCheckDelay {
		t.Fatalf("pending delay=%s", got)
	}
	a.state.Accounts["main"].Warmup.NextCheckAt = time.Time{}
	if got := a.nextRefreshDelay(); got != usageRefreshWindow {
		t.Fatalf("confirmed delay=%s", got)
	}
}

func TestVerificationAllowsUsageEndpointToCatchUp(t *testing.T) {
	now := time.Now()
	var w WarmupState
	w.begin(now)
	w.Phase = "verifying"
	w.observe(weeklyUsage(0, now.Add(weeklyWindow)), now)
	actualReset := now.Add(weeklyWindow + 5*time.Second)
	w.observe(weeklyUsage(0, actualReset), now.Add(warmupCheckDelay))
	if w.Phase != "verifying" {
		t.Fatal("should recheck a lagging baseline")
	}
	w.observe(weeklyUsage(0, actualReset), now.Add(2*warmupCheckDelay))
	if w.Phase != "confirmed" {
		t.Fatalf("state=%+v", w)
	}
}
