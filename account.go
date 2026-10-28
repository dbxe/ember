package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	usageEndpointBackend = "https://chatgpt.com/backend-api/codex/usage"
	usageEndpointAPI     = "https://chatgpt.com/api/codex/usage"
	usageEndpointWHAM    = "https://chatgpt.com/wham/usage"
	weeklyWindowMinutes  = 7 * 24 * 60
)

type AccountManager struct {
	homesPath string
}

type Account struct {
	Name string `json:"name"`
	Home string `json:"home"`
}

type AuthFile struct {
	Tokens AuthTokens `json:"tokens"`
}

type AuthTokens struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

type UsageSnapshot struct {
	WeeklyUsedPct *float64  `json:"weeklyUsedPct,omitempty"`
	WeeklyReset   time.Time `json:"weeklyReset,omitempty"`
	Source        string    `json:"source,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func NewAccountManager() *AccountManager {
	homeDir, _ := os.UserHomeDir()
	return &AccountManager{homesPath: filepath.Join(homeDir, ".codex", "ember", "homes.json")}
}

func (m *AccountManager) ListAccounts() ([]Account, error) {
	data, err := os.ReadFile(m.homesPath)
	if errors.Is(err, os.ErrNotExist) {
		data = []byte(`[{"name":"work","home":"~/.codex"}]`)
	} else if err != nil {
		return nil, err
	}
	var accounts []Account
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("homes.json: %w", err)
	}
	if accounts == nil {
		return nil, errors.New("homes.json: expected a list of Codex homes")
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if strings.TrimSpace(account.Name) == "" || names[account.Name] {
			return nil, errors.New("homes.json: each home needs a unique, nonempty name")
		}
		names[account.Name] = true
		if account.Home == "~" {
			account.Home = homeDir
		} else if strings.HasPrefix(account.Home, "~/") {
			account.Home = filepath.Join(homeDir, account.Home[2:])
		}
		if !filepath.IsAbs(account.Home) {
			return nil, fmt.Errorf("homes.json: home for %q must be an absolute path or start with ~/", account.Name)
		}
		account.Home = filepath.Clean(account.Home)
	}
	return accounts, nil
}

func (m *AccountManager) FetchUsage(account Account) (UsageSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(account.Home, "auth.json"))
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("read %s login: %w", account.Name, err)
	}
	var auth AuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return UsageSnapshot{}, fmt.Errorf("read %s login: %w", account.Name, err)
	}
	tokens := []struct {
		label string
		value string
	}{
		{label: "access_token", value: auth.Tokens.AccessToken},
		{label: "id_token", value: auth.Tokens.IDToken},
	}
	endpoints := []string{usageEndpointBackend, usageEndpointAPI, usageEndpointWHAM}

	for _, token := range tokens {
		if token.value == "" {
			continue
		}
		for _, endpoint := range endpoints {
			usage, err := fetchUsageEndpoint(endpoint, token.value, auth.Tokens.AccountID)
			if err == nil {
				usage.Source = endpoint + " via " + token.label
				return usage, nil
			}
		}
	}

	usage, err := m.fetchUsageFromServer(account)
	if err != nil {
		return UsageSnapshot{}, fmt.Errorf("weekly Codex usage unavailable: %w", err)
	}
	return usage, nil
}

// WarmAccount performs exactly one small inference in the configured Codex home.
// The caller verifies the reset countdown separately.
func (m *AccountManager) WarmAccount(account Account) error {
	root, err := os.MkdirTemp("", "ember-warm-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	replyPath := filepath.Join(root, "reply.txt")
	cmd := exec.CommandContext(ctx, codexCommandPath(), "exec",
		"--skip-git-repo-check", "--sandbox", "read-only", "--color", "never",
		"--model", "gpt-6-luna", "-c", `model_reasoning_effort="low"`,
		"--ephemeral", "--output-last-message", replyPath,
		"--", "Briefly acknowledge this message. Reply with exactly OK. Do not use tools.")
	cmd.Env = probeEnvironment(account.Home)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Codex warming probe failed (%v): %s", err, summarizeCodexMessage(stderr.String()))
	}
	reply, err := os.ReadFile(replyPath)
	if err != nil || strings.TrimSpace(string(reply)) == "" {
		return errors.New("Codex warming probe returned no acknowledgment")
	}
	return nil
}

// The fallback reads rate limits without generating a model response. Usage
// refreshes must not quietly warm an account or spend tokens themselves.
func (m *AccountManager) fetchUsageFromServer(account Account) (UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexCommandPath(), "app-server", "--listen", "stdio://")
	cmd.Env = probeEnvironment(account.Home)
	cmd.Dir = account.Home
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return UsageSnapshot{}, err
	}
	if err := cmd.Start(); err != nil {
		return UsageSnapshot{}, err
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	encoder := json.NewEncoder(stdin)
	send := func(id int, method string, params any) error {
		return encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
	}
	if err := send(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "ember", "version": "1.0"}}); err != nil {
		return UsageSnapshot{}, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID == 0 {
			continue
		}
		if response.Error != nil {
			return UsageSnapshot{}, errors.New(response.Error.Message)
		}
		if response.ID == 1 {
			if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
				return UsageSnapshot{}, err
			}
			if err := send(2, "account/rateLimits/read", nil); err != nil {
				return UsageSnapshot{}, err
			}
		}
		if response.ID == 2 {
			var result struct {
				RateLimits json.RawMessage            `json:"rateLimits"`
				ByID       map[string]json.RawMessage `json:"rateLimitsByLimitId"`
			}
			if err := json.Unmarshal(response.Result, &result); err != nil {
				return UsageSnapshot{}, err
			}
			raw := result.RateLimits
			if codex, ok := result.ByID["codex"]; ok {
				raw = codex
			}
			usage, err := parseUsageFromRateLimits(raw)
			usage.Source = "codex account/rateLimits/read"
			return usage, err
		}
	}
	if ctx.Err() != nil {
		return UsageSnapshot{}, ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return UsageSnapshot{}, err
	}
	return UsageSnapshot{}, errors.New("Codex exited without returning rate limits")
}

func probeEnvironment(home string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CODEX_HOME="+home)
}

func fetchUsageEndpoint(endpoint, token, accountID string) (UsageSnapshot, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return UsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ember/1.0")
	if accountID != "" {
		req.Header.Set("OpenAI-Account-ID", accountID)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return UsageSnapshot{}, fmt.Errorf("usage endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return UsageSnapshot{}, err
	}
	return parseUsagePayload(payload)
}

func parseUsagePayload(payload map[string]any) (UsageSnapshot, error) {
	if rateLimit, ok := payload["rate_limit"].(map[string]any); ok {
		if usage, err := parseUsageRateLimitPayload(rateLimit); err == nil {
			return usage, nil
		}
	}

	candidates := []any{payload}
	for _, key := range []string{"snapshots", "limits"} {
		if entries, ok := payload[key].([]any); ok {
			candidates = append(candidates, entries...)
		}
	}

	for _, candidate := range candidates {
		entry, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		name := strings.ToLower(firstString(entry["limitName"], entry["limit_name"], entry["limitId"], entry["limit_id"]))
		if !strings.Contains(name, "week") {
			continue
		}
		if usage, ok := usageFromWindow(entry); ok {
			return usage, nil
		}
	}

	return UsageSnapshot{}, errors.New("weekly usage fields not found")
}

func parseUsageRateLimitPayload(payload map[string]any) (UsageSnapshot, error) {
	type namedWindow struct {
		name   string
		window map[string]any
	}
	windows := make([]namedWindow, 0, 4)
	for _, name := range []string{"primary_window", "secondary_window", "primary", "secondary"} {
		if window, ok := payload[name].(map[string]any); ok {
			windows = append(windows, namedWindow{name: name, window: window})
		}
	}

	for _, candidate := range windows {
		minutes := windowDurationMinutes(candidate.window)
		isLegacyWeeklySecondary := minutes == 0 && strings.Contains(candidate.name, "secondary")
		if minutes >= weeklyWindowMinutes || isLegacyWeeklySecondary {
			if usage, ok := usageFromWindow(candidate.window); ok {
				return usage, nil
			}
		}
	}
	return UsageSnapshot{}, errors.New("weekly rate-limit window not found")
}

func usageFromWindow(window map[string]any) (UsageSnapshot, bool) {
	used := firstFloat(window["used_percent"], window["usedPercent"])
	if used == nil {
		if remaining := firstFloat(
			window["remaining_percent"], window["remainingPercent"],
			window["percent_remaining"], window["percentRemaining"], window["remainingPercentage"],
		); remaining != nil {
			value := 100 - *remaining
			used = &value
		}
	}
	if used == nil {
		return UsageSnapshot{}, false
	}
	value := max(0, min(100, *used))
	usage := UsageSnapshot{WeeklyUsedPct: &value}
	usage.WeeklyReset = firstTime(window["reset_at"], window["resetAt"], window["resets_at"], window["resetsAt"], window["refresh_at"], window["refreshAt"])
	if usage.WeeklyReset.IsZero() {
		usage.WeeklyReset = firstUnix(window["reset_at"], window["resetAt"], window["resets_at"], window["resetsAt"])
	}
	return usage, true
}

func windowDurationMinutes(window map[string]any) int {
	if minutes := firstFloat(window["window_minutes"], window["windowMinutes"], window["windowDurationMins"]); minutes != nil {
		return int(*minutes)
	}
	if seconds := firstFloat(window["window_seconds"], window["windowSeconds"], window["limit_window_seconds"]); seconds != nil {
		return int(*seconds / 60)
	}
	return 0
}

func parseUsageFromRateLimits(raw json.RawMessage) (UsageSnapshot, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return UsageSnapshot{}, err
	}
	return parseUsageRateLimitPayload(payload)
}

func firstString(values ...any) string {
	for _, value := range values {
		if str, ok := value.(string); ok && str != "" {
			return str
		}
	}
	return ""
}

func firstFloat(values ...any) *float64 {
	for _, value := range values {
		switch typed := value.(type) {
		case float64:
			v := typed
			return &v
		case int:
			v := float64(typed)
			return &v
		case json.Number:
			if parsed, err := typed.Float64(); err == nil {
				return &parsed
			}
		}
	}
	return nil
}

func firstTime(values ...any) time.Time {
	for _, value := range values {
		str, ok := value.(string)
		if !ok || str == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, str); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func firstUnix(values ...any) time.Time {
	for _, value := range values {
		if number := firstFloat(value); number != nil && *number > 0 {
			return time.Unix(int64(*number), 0)
		}
	}
	return time.Time{}
}

func fileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && !info.IsDir()
}

func summarizeCodexMessage(raw string) string {
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "warning: proceeding, even though we could not update path"):
			continue
		case strings.HasPrefix(lower, "reading additional input from stdin"):
			continue
		case strings.HasPrefix(lower, "openai codex v"):
			continue
		case line == "--------":
			continue
		case strings.HasPrefix(lower, "workdir:"),
			strings.HasPrefix(lower, "model:"),
			strings.HasPrefix(lower, "provider:"),
			strings.HasPrefix(lower, "approval:"),
			strings.HasPrefix(lower, "sandbox:"),
			strings.HasPrefix(lower, "reasoning effort:"),
			strings.HasPrefix(lower, "reasoning summaries:"),
			strings.HasPrefix(lower, "session id:"),
			lower == "user",
			lower == "codex",
			strings.HasPrefix(lower, "tokens used"):
			continue
		default:
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

func codexCommandPath() string {
	lookedUp, err := exec.LookPath("codex")
	if err != nil {
		if fallback := findCodexFallback(); fallback != "" {
			return fallback
		}
		return "codex"
	}

	resolved, err := filepath.EvalSymlinks(lookedUp)
	if err != nil {
		resolved = lookedUp
	}
	target := codexTargetTriple()
	if target == "" {
		return lookedUp
	}

	baseDir := filepath.Dir(resolved)
	candidates := []string{
		filepath.Clean(filepath.Join(baseDir, "..", "vendor", target, "codex", "codex")),
		filepath.Clean(filepath.Join(baseDir, "..", "node_modules", "@openai", "codex-"+platformPackageSuffix(), "vendor", target, "codex", "codex")),
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	if fallback := findCodexFallback(); fallback != "" {
		return fallback
	}
	return lookedUp
}

func codexTargetTriple() string {
	switch runtime.GOOS {
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			return "aarch64-apple-darwin"
		case "amd64":
			return "x86_64-apple-darwin"
		}
	case "linux":
		switch runtime.GOARCH {
		case "arm64":
			return "aarch64-unknown-linux-musl"
		case "amd64":
			return "x86_64-unknown-linux-musl"
		}
	case "windows":
		switch runtime.GOARCH {
		case "arm64":
			return "aarch64-pc-windows-msvc"
		case "amd64":
			return "x86_64-pc-windows-msvc"
		}
	}
	return ""
}

func platformPackageSuffix() string {
	switch runtime.GOOS {
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			return "darwin-arm64"
		case "amd64":
			return "darwin-x64"
		}
	case "linux":
		switch runtime.GOARCH {
		case "arm64":
			return "linux-arm64"
		case "amd64":
			return "linux-x64"
		}
	case "windows":
		switch runtime.GOARCH {
		case "arm64":
			return "win32-arm64"
		case "amd64":
			return "win32-x64"
		}
	}
	return ""
}

func findCodexFallback() string {
	target := codexTargetTriple()
	suffix := platformPackageSuffix()
	homeDir, _ := os.UserHomeDir()

	candidates := []string{
		filepath.Join(homeDir, "bin", "codex"),
		filepath.Join(homeDir, ".local", "bin", "codex"),
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
	}
	if target != "" && suffix != "" && homeDir != "" {
		nvmGlob := filepath.Join(homeDir, ".nvm", "versions", "node", "*", "lib", "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-"+suffix, "vendor", target, "codex", "codex")
		if matches, err := filepath.Glob(nvmGlob); err == nil {
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				candidates = append([]string{matches[i]}, candidates...)
			}
		}
	}

	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}
