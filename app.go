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
	refreshInterval    = 10 * time.Minute
)

type PersistentState struct {
	Accounts map[string]*AccountCache `json:"accounts"`
}

type AccountCache struct {
	Identity           string        `json:"identity,omitempty"`
	LastUsageFetchedAt time.Time     `json:"lastUsageFetchedAt,omitempty"`
	Usage              UsageSnapshot `json:"usage"`
}

type AccountView struct {
	Account Account
	Active  bool
	Cache   AccountCache
}

type App struct {
	mu          sync.RWMutex
	syncMu      sync.Mutex
	menuMu      sync.Mutex
	manager     *AccountManager
	state       PersistentState
	accounts    []AccountView
	activeName  string
	lastError   string
	menuDirty   bool
	stopCh      chan struct{}
	stopOnce    sync.Once
	backgrounds sync.WaitGroup
}

func NewApp() *App {
	return &App{
		manager: NewAccountManager(),
		state: PersistentState{
			Accounts: map[string]*AccountCache{},
		},
		stopCh: make(chan struct{}),
	}
}

func (a *App) Start() {
	if err := a.loadState(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to load Ember state: %v\n", err)
	}
	if err := a.manager.PersistCurrentAuth(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to persist current auth: %v\n", err)
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
	if err := a.manager.PersistCurrentAuth(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to persist current auth: %v\n", err)
	}
	RemoveStatusBar()
}

func (a *App) HandleMenuAction(tag int) {
	switch {
	case tag == TagRefresh:
		if err := a.refreshAccounts(true); err != nil {
			a.setLastError(err)
			fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
		} else {
			a.setLastError(nil)
		}
	case tag == TagQuit:
		TerminateApp()
	case tag >= TagAccountBase && tag < TagDetailsBase:
		index := tag - TagAccountBase
		view, ok := a.accountAt(index)
		if !ok || view.Active {
			return
		}
		if err := a.switchToAccount(view.Account.Name); err != nil {
			a.setLastError(fmt.Errorf("switch to %s: %w", view.Account.Name, err))
			break
		}
		if err := a.refreshAccounts(false); err != nil {
			a.setLastError(err)
			fmt.Fprintf(os.Stderr, "refresh after switch failed: %v\n", err)
		} else {
			a.setLastError(nil)
		}
	}

	a.rebuildMenu()
}

func (a *App) switchToAccount(name string) error {
	// Usage probes can rotate a snapshot's refresh token. Serialize switching
	// with those probes so PersistCurrentAuth cannot overwrite the rotated copy.
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.manager.SwitchToAccount(name)
}

func (a *App) refreshLoop() {
	defer a.backgrounds.Done()

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := a.refreshAccounts(false); err != nil {
				a.setLastError(err)
				fmt.Fprintf(os.Stderr, "refresh failed: %v\n", err)
			} else {
				a.setLastError(nil)
			}
			a.rebuildMenu()
		case <-a.stopCh:
			return
		}
	}
}

func (a *App) accountAt(index int) (AccountView, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if index < 0 || index >= len(a.accounts) {
		return AccountView{}, false
	}
	return a.accounts[index], true
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

	accounts, activeName, err := a.manager.ListAccounts()
	if err != nil {
		return err
	}

	now := time.Now()
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

		identity := account.AccountID
		if identity == "" {
			identity = account.Email
		}
		if cache.Identity != identity {
			cache = AccountCache{Identity: identity}
		}

		if fetchUsage && (forceUsage || cache.LastUsageFetchedAt.IsZero() || now.Sub(cache.LastUsageFetchedAt) >= usageRefreshWindow) {
			usage, usageErr := a.manager.FetchUsage(account)
			cache.LastUsageFetchedAt = now
			if usageErr == nil {
				cache.Usage = usage
			} else {
				cache.Usage = UsageSnapshot{Error: usageErr.Error()}
			}
		}

		updatedCaches[account.Name] = &cache
		views = append(views, AccountView{
			Account: account,
			Active:  account.Name == activeName,
			Cache:   cache,
		})
	}

	a.mu.Lock()
	a.accounts = views
	a.activeName = activeName
	a.state.Accounts = updatedCaches
	a.mu.Unlock()

	a.updateStatusTitle()
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

func (a *App) updateStatusTitle() {
	a.mu.RLock()
	active := a.activeName
	count := len(a.accounts)
	a.mu.RUnlock()

	title := "Ember"
	switch {
	case active != "":
		title = "Ember " + active
	case count == 0:
		title = "Ember ?"
	}
	SetStatusTitle(title)
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
	active := a.activeName
	lastError := a.lastError
	a.mu.RUnlock()
	entries := buildMenuEntries(accounts, active, lastError)

	ClearMenu()
	for _, entry := range entries {
		if entry.Separator {
			AddSeparator()
		} else {
			addMenuItem(entry.Title, entry.Tag, entry.Enabled, entry.Checked)
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
	Checked   bool
	Separator bool
}

func buildMenuEntries(accounts []AccountView, active, lastError string) []menuEntry {
	entries := make([]menuEntry, 0, 5+len(accounts)*3)
	header := "No Codex accounts saved in ~/.codex/accounts"
	if active != "" {
		header = "Active account: " + active
	}
	entries = append(entries, menuEntry{Title: header, Tag: TagHeader})
	if lastError != "" {
		entries = append(entries, menuEntry{Title: "  " + lastError, Tag: TagDetailsBase})
	}
	entries = append(entries, menuEntry{Separator: true})

	for i, account := range accounts {
		entries = append(entries, menuEntry{
			Title:   fmt.Sprintf("%s    %s", account.Account.Name, formatWeeklyUsage(account.Cache.Usage)),
			Tag:     TagAccountBase + i,
			Enabled: true,
			Checked: account.Active,
		})
		if subtitle := buildAccountSubtitle(account.Account); subtitle != "" {
			entries = append(entries, menuEntry{Title: "  " + subtitle, Tag: TagDetailsBase + 2*i + 1})
		}
		entries = append(entries, menuEntry{
			Title: "  " + formatWeeklyReset(account.Cache.Usage),
			Tag:   TagDetailsBase + 2*i + 2,
		})
	}

	if len(accounts) > 0 {
		entries = append(entries, menuEntry{Separator: true})
	}
	entries = append(entries,
		menuEntry{Title: "Refresh usage", Tag: TagRefresh, Enabled: true},
		menuEntry{Separator: true},
		menuEntry{Title: "Quit " + AppName, Tag: TagQuit, Enabled: true},
	)
	return entries
}

func buildAccountSubtitle(account Account) string {
	return strings.TrimSpace(strings.Join([]string{account.Email, account.Plan}, " "))
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
