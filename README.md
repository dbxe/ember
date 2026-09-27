# Ember

Ember is a tiny native macOS menu bar app for switching between local Codex accounts and seeing each account's weekly usage.

## What it does

- manages saved `~/.codex/accounts/*.json` snapshots
- switches the active local Codex account without symlink-based auth churn
- shows the percentage of each account's weekly Codex allowance that has been used
- shows each account's weekly reset date and time in your local timezone, or indicates when it is unavailable
- automatically warms cold accounts after their weekly allowance resets

Ember persists the active `~/.codex/auth.json` back into its named snapshot before switching, then atomically copies the selected snapshot into place. This preserves refresh-token rotation without changing normal `codex login` or `codex logout` behavior.

Before each usage refresh, Ember also synchronizes the active credentials into their snapshot so it picks up logins renewed by Codex while Ember is running. If a Codex usage read or warming probe renews credentials for the active account, Ember updates both copies, including when the request fails after renewal.

Weekly usage refreshes every ten minutes and can be refreshed manually from the menu. If the usage endpoint is unavailable for local snapshot credentials, Ember reads rate limits through an isolated Codex app server. Usage checks do not generate model responses.

After a weekly reset, an unused account can show 0% usage with a reset time that keeps moving exactly seven days ahead. Ember checks again after 90 seconds to confirm that the reset is moving, then sends one small acknowledgment request to `gpt-6-luna`. It checks the reset again after at least 90 seconds and confirms warming only when the timestamp stays fixed and starts counting down. Accounts already counting down are left alone, even when usage still rounds to 0%.

Warming works for active and inactive accounts without switching your current account. The probe uses a temporary home with only the account credentials, a read-only sandbox, and no inherited user configuration or API key. Ember shows warming progress or retry errors in the menu, persists attempts across restarts, and limits failed attempts to at most one per hour. After confirmation, it returns to the normal ten-minute usage schedule. Codex CLI must be installed and support `gpt-6-luna`.

## Add another account

Quit Ember, then run:

```bash
make add-account NAME=work
```

The helper preserves the current account snapshot, runs the normal `codex logout` and `codex login` flow, and saves the new login as `~/.codex/accounts/work.json`. If login is canceled or fails, it restores the previous login.

After the helper finishes, start Ember again. The new account appears in the menu and is active. Click either account row to switch.

Account names may contain letters, numbers, dots, underscores, and hyphens. The helper refuses to overwrite an existing account.

If an existing account's login expires or is revoked, quit Ember and reauthenticate it in place:

```bash
make reauth-account NAME=work
```

## Build

```bash
go build ./...
make bundle
```

## Install

```bash
make install
```

## Verify

Run the automated tests with `go test -race ./...` and `go vet ./...`.

For opt-in checks against the saved accounts, use `EMBER_LIVE_OBSERVE=1 go test -run TestLiveObserve -count=1 -v` to print live usage and reset timestamps, or `EMBER_LIVE_SERVER=1 go test -run TestLiveReadServer -count=1 -v` to check the read-only fallback. Neither check sends a model prompt; the fallback can renew saved credentials, so quit Ember before running that check.
