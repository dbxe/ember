package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	AppName     = "Ember"
	AppSlug     = "ember"
	AppBundleID = "io.dbxf.ember"

	usageRefreshWindow = 10 * time.Minute
)

type PersistentState struct {
	Accounts map[string]*AccountCache `json:"accounts"`
}

type AccountCache struct {
	LastUsageFetchedAt time.Time     `json:"lastUsageFetchedAt,omitempty"`
	Usage              UsageSnapshot `json:"usage"`
	Warmup             WarmupState   `json:"warmup,omitempty"`
}

type AccountView struct {
	Account Account
	Cache   AccountCache
}

type App struct {
	mu          sync.RWMutex
	syncMu      sync.Mutex
	menuMu      sync.Mutex
	manager     *AccountManager
	now         func() time.Time
	state       PersistentState
	accounts    []AccountView
	lastError   string
	menuDirty   bool
	stopCh      chan struct{}
	refreshWake chan struct{}
	stopOnce    sync.Once
	backgrounds sync.WaitGroup
}

func NewApp() *App {
	return &App{
		manager: NewAccountManager(),
		now:     time.Now,
		state: PersistentState{
			Accounts: map[string]*AccountCache{},
		},
		stopCh:      make(chan struct{}),
		refreshWake: make(chan struct{}, 1),
	}
}

func (a *App) Start() {
	if err := a.loadState(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to load Ember state: %v\n", err)
	}

	SetupStatusBar(a)
	if err := a.loadAccountsFromDisk(); err != nil {
		a.setLastError(err)
		fmt.Fprintf(os.Stderr, "failed to load accounts: %v\n", err)
	}
	a.rebuildMenu()

	a.backgrounds.Add(2)
	go func() {
		defer a.backgrounds.Done()
		if err := a.refreshAccounts(true); err != nil {
			a.setLastError(err)
			fmt.Fprintf(os.Stderr, "failed to refresh accounts: %v\n", err)
		}
		a.rebuildMenu()
	}()
	go a.refreshLoop()
}

func (a *App) Shutdown() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
	a.backgrounds.Wait()

	RemoveStatusBar()
}

func (a *App) HandleMenuAction(tag int) {
	switch tag {
	case TagRefresh:
		if err := a.refreshAccounts(true); err != nil {
			a.setLastError(err)
			fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
		} else {
			a.setLastError(nil)
		}
	case TagQuit:
		TerminateApp()
	}

	a.rebuildMenu()
}

func (a *App) refreshLoop() {
	defer a.backgrounds.Done()

	timer := time.NewTimer(a.nextRefreshDelay())
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			if err := a.refreshAccounts(false); err != nil {
				a.setLastError(err)
				fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
			} else {
				a.setLastError(nil)
			}
			a.rebuildMenu()
		case <-a.refreshWake:
		case <-a.stopCh:
			return
		}
		timer.Reset(a.nextRefreshDelay())
	}
}

func (a *App) nextRefreshDelay() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := a.now()
	next := now.Add(usageRefreshWindow)
	for _, cache := range a.state.Accounts {
		due := cache.LastUsageFetchedAt.Add(usageRefreshWindow)
		if due.Before(next) {
			next = due
		}
		if check := cache.Warmup.NextCheckAt; !check.IsZero() && check.Before(next) {
			next = check
		}
	}
	return max(time.Second, next.Sub(now))
}

func (a *App) refreshAccounts(forceUsage bool) error {
	return a.syncAccounts(true, forceUsage)
}

func (a *App) loadAccountsFromDisk() error {
	return a.syncAccounts(false, false)
}

func (a *App) syncAccounts(fetchUsage, forceUsage bool) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	accounts, err := a.manager.ListAccounts()
	if err != nil {
		return err
	}

	now := a.now()
	views := make([]AccountView, 0, len(accounts))
	updatedCaches := make(map[string]*AccountCache, len(accounts))
	for _, account := range accounts {
		a.mu.RLock()
		cached := a.state.Accounts[account.Name]
		cache := AccountCache{}
		if cached != nil {
			cache = *cached
		}
		a.mu.RUnlock()

		if fetchUsage && (forceUsage || cache.LastUsageFetchedAt.IsZero() || now.Sub(cache.LastUsageFetchedAt) >= usageRefreshWindow || (!cache.Warmup.NextCheckAt.IsZero() && !now.Before(cache.Warmup.NextCheckAt))) {
			usage, usageErr := a.manager.FetchUsage(account)
			cache.LastUsageFetchedAt = a.now()
			if usageErr == nil {
				cache.Usage = usage
				if cache.Warmup.observe(usage, a.now()) {
					cache.Warmup.begin(a.now())
					// Record the attempt before starting inference, including across crashes.
					a.mu.Lock()
					saved := cache
					a.state.Accounts[account.Name] = &saved
					a.mu.Unlock()
					if err := a.saveState(); err != nil {
						return err
					}
					if err := a.manager.WarmAccount(account); err != nil {
						cache.Warmup.fail(err.Error(), a.now())
					} else {
						cache.Warmup.Phase = "verifying"
						if after, fetchErr := a.manager.FetchUsage(account); fetchErr == nil {
							cache.Usage = after
							cache.LastUsageFetchedAt = a.now()
							cache.Warmup.observe(after, a.now())
						}
					}
				}
			} else {
				cache.Usage = UsageSnapshot{Error: usageErr.Error()}
				if !cache.Warmup.NextCheckAt.IsZero() {
					cache.Warmup.NextCheckAt = a.now().Add(usageRefreshWindow)
				}
			}
		}

		updatedCaches[account.Name] = &cache
		views = append(views, AccountView{
			Account: account,
			Cache:   cache,
		})
	}

	a.mu.Lock()
	a.accounts = views
	a.state.Accounts = updatedCaches
	a.mu.Unlock()

	if fetchUsage {
		select {
		case a.refreshWake <- struct{}{}:
		default:
		}
	}
	return a.saveState()
}

func (a *App) setLastError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastError = ""
		return
	}
	a.lastError = trimForMenu(err.Error())
}

func (a *App) rebuildMenu() {
	a.menuMu.Lock()
	defer a.menuMu.Unlock()

	if StatusMenuIsOpen() {
		a.mu.Lock()
		a.menuDirty = true
		a.mu.Unlock()
		return
	}

	a.mu.RLock()
	accounts := append([]AccountView(nil), a.accounts...)
	lastError := a.lastError
	a.mu.RUnlock()
	entries := buildMenuEntries(accounts, lastError)

	ClearMenu()
	for _, entry := range entries {
		if entry.Separator {
			AddSeparator()
		} else {
			addMenuItem(entry.Title, entry.Tag, entry.Enabled)
		}
	}

	a.mu.Lock()
	a.menuDirty = false
	a.mu.Unlock()
}

func (a *App) rebuildMenuIfNeeded() {
	a.mu.RLock()
	dirty := a.menuDirty
	a.mu.RUnlock()
	if dirty {
		a.rebuildMenu()
	}
}

type menuEntry struct {
	Title     string
	Tag       int
	Enabled   bool
	Separator bool
}

func buildMenuEntries(accounts []AccountView, lastError string) []menuEntry {
	entries := make([]menuEntry, 0, 5+len(accounts)*4)
	if len(accounts) == 0 {
		entries = append(entries, menuEntry{Title: "No Codex homes configured"})
	}
	if lastError != "" {
		entries = append(entries, menuEntry{Title: trimForMenu(lastError)})
	}
	for _, account := range accounts {
		entries = append(entries,
			menuEntry{Title: fmt.Sprintf("%s    %s", account.Account.Name, formatWeeklyUsage(account.Cache.Usage))},
			menuEntry{Title: "  " + formatWeeklyReset(account.Cache.Usage)},
		)
		if message := account.Cache.Usage.Error; message != "" {
			entries = append(entries, menuEntry{Title: "  " + trimForMenu(message)})
		}
		if subtitle := account.Cache.Warmup.subtitle(); subtitle != "" {
			entries = append(entries, menuEntry{Title: "  " + subtitle})
		}
	}
	entries = append(entries,
		menuEntry{Separator: true},
		menuEntry{Title: "Refresh usage", Tag: TagRefresh, Enabled: true},
		menuEntry{Separator: true},
		menuEntry{Title: "Quit " + AppName, Tag: TagQuit, Enabled: true},
	)
	return entries
}

func formatWeeklyUsage(usage UsageSnapshot) string {
	if usage.WeeklyUsedPct == nil {
		return "Weekly --"
	}
	return fmt.Sprintf("Weekly %.0f%% used", *usage.WeeklyUsedPct)
}

func formatWeeklyReset(usage UsageSnapshot) string {
	if usage.WeeklyReset.IsZero() {
		return "Weekly reset unavailable"
	}
	return "Weekly resets " + usage.WeeklyReset.Local().Format("Mon, Jan 2, 2006 at 15:04 MST")
}

func trimForMenu(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 72 {
		return value
	}
	return value[:69] + "..."
}

func (a *App) loadState() error {
	path, err := emberStatePath()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Accounts == nil {
		state.Accounts = map[string]*AccountCache{}
	}

	a.mu.Lock()
	a.state = state
	a.mu.Unlock()
	return nil
}

func (a *App) saveState() error {
	path, err := emberStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	a.mu.RLock()
	data, err := json.MarshalIndent(a.state, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func emberStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "ember", "state.json"), nil
}
