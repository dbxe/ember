# Ember

Ember is a native macOS menu bar app that shows weekly Codex usage per account and keeps accounts warm. Each account lives in its own Codex home. The menu shows its name, weekly percentage used, reset date in your local timezone, and warming progress or errors. Refresh usage and Quit are the only menu actions.

## Configure accounts

Create `~/.codex/ember/homes.json` with a list of named Codex homes:

```json
[
  {"name": "work", "home": "~/.codex"},
  {"name": "main", "home": "~/.codex_main"}
]
```

Names must be nonempty and unique. Home paths must be absolute or start with `~/`; `~` expands to your home directory. If the file is missing, Ember uses only `{"name":"work","home":"~/.codex"}`. An empty list shows no accounts, and malformed configuration appears as an error in the menu. Refresh usage reloads the list.

To add an account, log in to a separate home, then add its entry to `homes.json`:

```bash
mkdir -p ~/.codex_main && CODEX_HOME=~/.codex_main codex login
```

To reauthenticate an account, run `CODEX_HOME=<home> codex login`, replacing `<home>` with that account's home path. For example:

```bash
CODEX_HOME=~/.codex_main codex login
```

Ember never writes credentials. It reads each home's `auth.json` for direct usage requests; when those requests fail or the login file cannot be read or parsed, the usage fallback runs `codex app-server --listen stdio://` in the home with that home's `CODEX_HOME` to read `account/rateLimits/read`. Codex itself may renew credentials in that home. Usage reads do not generate model responses.

`~/.codex/accounts/` is no longer read and can be deleted once the accounts live in their own homes.

## Warming

Weekly usage refreshes every ten minutes and can also be refreshed from the menu. After a reset, an unused account can show 0% usage with a reset time that keeps moving seven days ahead. Ember checks again after 90 seconds to confirm movement, then sends one small acknowledgment request to `gpt-6-luna`. It checks again after at least 90 seconds and confirms warming only when the reset stays fixed and starts counting down. Accounts already counting down are left alone, even when usage rounds to 0%.

The warming probe runs `codex exec` in the account's real Codex home with `--ephemeral` and a read-only sandbox. The warming probe uses `--ignore-user-config` to ignore the home's `config.toml`, so it does not start the home's MCP servers, hooks, or plugins. Its working directory is a fresh empty temporary directory, removed afterward, containing only the acknowledgment output. Both usage fallback and warming preserve `HOME`, set `CODEX_HOME` to the configured home, and strip `OPENAI_API_KEY`, `CODEX_API_KEY`, and `OPENAI_BASE_URL`.

Ember persists warming state by account name in `~/.codex/ember/state.json`, including attempts across restarts. Failed probes retry at most once per hour. After confirmation, Ember returns to the ten-minute usage schedule. Codex CLI must be installed and support `gpt-6-luna`.

## Build and install

```bash
go build ./...
make bundle
make install
```

## Verify

```bash
gofmt -l .
go vet ./...
go test -race ./...
go build ./...
make bundle
```

The automated tests use synthetic homes and a fake Codex executable, without reading real credentials or calling live services.
