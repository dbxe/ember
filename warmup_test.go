package main

import (
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
