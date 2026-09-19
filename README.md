# Ember

Ember is a tiny native macOS menu bar app for switching between local Codex accounts and seeing each account's weekly usage.

## What it does

- manages saved `~/.codex/accounts/*.json` snapshots
- switches the active local Codex account without symlink-based auth churn
- shows the percentage of each account's weekly Codex allowance that has been used
- shows each account's weekly reset date and time in your local timezone, or indicates when it is unavailable

Ember persists the active `~/.codex/auth.json` back into its named snapshot before switching, then atomically copies the selected snapshot into place. This preserves refresh-token rotation without changing normal `codex login` or `codex logout` behavior.

Weekly usage refreshes every ten minutes and can be refreshed manually from the menu. If the usage endpoint is unavailable for local snapshot credentials, Ember runs a minimal isolated Codex request and reads the weekly rate-limit data from that session.

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
