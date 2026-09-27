package main

import (
	"os"
	"testing"
	"time"
)

func TestLiveObserve(t *testing.T) {
	if os.Getenv("EMBER_LIVE_OBSERVE") != "1" {
		t.Skip("opt-in live account observation")
	}
	m := NewAccountManager()
	accounts, _, err := m.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		u, err := fetchUsageEndpoint(usageEndpointBackend, a.Auth.Tokens.AccessToken, a.Auth.Tokens.AccountID)
		if err != nil {
			t.Errorf("%s: %v", a.Name, err)
			continue
		}
		t.Logf("%s: now=%s used=%v reset=%s remaining=%s", a.Name, time.Now().Format(time.RFC3339), *u.WeeklyUsedPct, u.WeeklyReset.Format(time.RFC3339), time.Until(u.WeeklyReset))
	}
}

func TestLiveReadServer(t *testing.T) {
	if os.Getenv("EMBER_LIVE_SERVER") != "1" {
		t.Skip("opt-in live read-only fallback")
	}
	m := NewAccountManager()
	accounts, _, err := m.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		u, err := m.fetchUsageFromServer(a)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: used=%v reset=%s", a.Name, *u.WeeklyUsedPct, u.WeeklyReset.Format(time.RFC3339))
	}
}
