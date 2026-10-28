package main

import (
	"encoding/json"
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
