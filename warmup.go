package main

import "time"

const (
	weeklyWindow     = 7 * 24 * time.Hour
	warmupCheckDelay = 90 * time.Second
	warmupRetryDelay = time.Hour
)

// WarmupState is persisted so manual refreshes and restarts cannot cause a
// burst of probes. Two observations distinguish an idle sliding window from
// a newly started window whose usage still rounds to zero.
type WarmupState struct {
	Phase         string    `json:"phase,omitempty"`
	ObservedAt    time.Time `json:"observedAt,omitempty"`
	Reset         time.Time `json:"reset,omitempty"`
	NextCheckAt   time.Time `json:"nextCheckAt,omitempty"`
	LastAttemptAt time.Time `json:"lastAttemptAt,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func coldWeeklyUsage(usage UsageSnapshot, now time.Time) bool {
	if usage.Error != "" || usage.WeeklyUsedPct == nil || *usage.WeeklyUsedPct != 0 || usage.WeeklyReset.IsZero() {
		return false
	}
	remaining := usage.WeeklyReset.Sub(now)
	return remaining >= weeklyWindow-time.Minute && remaining <= weeklyWindow+time.Minute
}

// observe returns true only when a fresh pair of samples proves that the
// zero-usage reset is moving with the clock and the retry cooldown has elapsed.
func (w *WarmupState) observe(usage UsageSnapshot, now time.Time) bool {
	if usage.Error != "" || usage.WeeklyUsedPct == nil || !usage.WeeklyReset.After(now) {
		if !w.NextCheckAt.IsZero() {
			w.NextCheckAt = now.Add(usageRefreshWindow)
		}
		return false
	}
	if w.Phase == "probing" {
		w.fail("Previous warming probe was interrupted", now)
		return false
	}
	if w.Phase == "verifying" {
		if w.ObservedAt.IsZero() {
			w.ObservedAt, w.Reset = now, usage.WeeklyReset
			w.NextCheckAt = now.Add(warmupCheckDelay)
			return false
		}
		if now.Sub(w.ObservedAt) < warmupCheckDelay {
			return false
		}
		if usage.WeeklyReset.Equal(w.Reset) && usage.WeeklyReset.Sub(now) < weeklyWindow-time.Minute {
			w.Phase, w.Error = "confirmed", ""
			w.NextCheckAt = time.Time{}
		} else if now.Sub(w.LastAttemptAt) < usageRefreshWindow {
			// The usage endpoint can lag behind the acknowledgment. Establish a
			// new baseline before deciding whether the reset is still sliding.
			w.ObservedAt, w.Reset = now, usage.WeeklyReset
			w.NextCheckAt = now.Add(warmupCheckDelay)
		} else {
			w.fail("Reset has not started counting down", now)
		}
		return false
	}
	if !coldWeeklyUsage(usage, now) {
		if w.Phase == "confirmed" && usage.WeeklyReset.Equal(w.Reset) {
			return false
		}
		w.Phase, w.Error = "", ""
		w.NextCheckAt = time.Time{}
		w.ObservedAt, w.Reset = time.Time{}, time.Time{}
		return false
	}
	if !w.LastAttemptAt.IsZero() && now.Before(w.LastAttemptAt.Add(warmupRetryDelay)) {
		return false
	}
	if w.Phase == "checking" && now.Sub(w.ObservedAt) >= warmupCheckDelay {
		elapsed := now.Sub(w.ObservedAt)
		shift := usage.WeeklyReset.Sub(w.Reset)
		// Allow minute-resolution timestamps, but require actual advancement.
		if shift >= time.Minute && shift >= elapsed-time.Minute && shift <= elapsed+time.Minute {
			return true
		}
	}
	if w.Phase != "checking" || now.Sub(w.ObservedAt) >= warmupCheckDelay {
		w.Phase, w.Error = "checking", ""
		w.ObservedAt, w.Reset = now, usage.WeeklyReset
		w.NextCheckAt = now.Add(warmupCheckDelay)
	}
	return false
}

func (w *WarmupState) begin(now time.Time) {
	w.Phase, w.Error = "probing", ""
	w.LastAttemptAt = now
	w.ObservedAt, w.Reset = time.Time{}, time.Time{}
	w.NextCheckAt = now.Add(warmupCheckDelay)
}

func (w *WarmupState) fail(message string, now time.Time) {
	w.Phase, w.Error = "retry", message
	w.NextCheckAt = now.Add(warmupRetryDelay)
}

func (w WarmupState) subtitle() string {
	switch w.Phase {
	case "checking":
		return "Checking whether account needs warming"
	case "probing":
		return "Warming account"
	case "verifying":
		return "Confirming weekly reset countdown"
	case "confirmed":
		return "Account warmed; reset countdown confirmed"
	case "retry":
		return trimForMenu("Warming will retry: " + w.Error)
	}
	return ""
}
