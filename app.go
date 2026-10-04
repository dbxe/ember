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
	Identity           string        `json:"identity,omitempty"`
	LastUsageFetchedAt time.Time     `json:"lastUsageFetchedAt,omitempty"`
	Usage              UsageSnapshot `json:"usage"`
	Warmup             WarmupState   `json:"warmup,omitempty"`
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
	now         func() time.Time
	state       PersistentState
	accounts    []AccountView
	activeName  string
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
	if err := a.manager.PersistCurrentAuth(); err != nil {
		a.setLastError(err)
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

	// Codex can renew the active login while Ember is running. Read that
	// renewal before loading the snapshots used by either refresh path.
	var syncErr error
	if fetchUsage {
		if err := a.manager.PersistCurrentAuth(); err != nil {
			syncErr = fmt.Errorf("persist current auth before usage refresh: %w", err)
			fetchUsage = false
		}
	}

	accounts, activeName, err := a.manager.ListAccounts()
	if err != nil {
		return err
	}
	// The marker may be stale after a login outside Ember. Do not put a
	// checkmark on credentials that are no longer the active login.
	if activeName != "" {
		active, err := a.manager.readAccount(activeName, a.manager.authPath)
		verified := false
		if err == nil {
			for _, account := range accounts {
				if account.Name == activeName && account.identity() == active.identity() {
					verified = true
				}
			}
		}
		if !verified {
			activeName = ""
		}
	}
	owners := loginOwners(accounts, activeName)

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

		identity := account.identity()
		if cache.Identity != identity {
			cache = AccountCache{Identity: identity}
		}
		if syncErr != nil {
			cache.LastUsageFetchedAt = now
			if !cache.Warmup.NextCheckAt.IsZero() {
				cache.Warmup.NextCheckAt = now.Add(usageRefreshWindow)
			}
		}

		if owner := owners[identity]; identity != "" && owner != account.Name {
			// Aliases share one allowance. Only its owner can fetch or warm it.
			cache = AccountCache{Identity: identity, LastUsageFetchedAt: now}
		} else if fetchUsage && (forceUsage || cache.LastUsageFetchedAt.IsZero() || now.Sub(cache.LastUsageFetchedAt) >= usageRefreshWindow || (!cache.Warmup.NextCheckAt.IsZero() && !now.Before(cache.Warmup.NextCheckAt))) {
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
					// A read-only fallback may have rotated this account's credentials.
					fresh, err := a.manager.readAccount(account.Name, account.Path)
					if err == nil {
						err = a.manager.WarmAccount(fresh)
					}
					if err != nil {
						cache.Warmup.fail(err.Error(), a.now())
					} else {
						cache.Warmup.Phase = "verifying"
						fresh, err = a.manager.readAccount(account.Name, account.Path)
						if err == nil {
							if after, fetchErr := a.manager.FetchUsage(fresh); fetchErr == nil {
								cache.Usage = after
								cache.LastUsageFetchedAt = a.now()
								cache.Warmup.observe(after, a.now())
							}
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
	if fetchUsage {
		select {
		case a.refreshWake <- struct{}{}:
		default:
		}
	}
	return errors.Join(syncErr, a.saveState())
}

func loginOwners(accounts []Account, active string) map[string]string {
	owners := make(map[string]string)
	for _, account := range accounts {
		identity := account.identity()
		if identity != "" && (owners[identity] == "" || account.Name == active) {
			owners[identity] = account.Name
		}
	}
	return owners
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
	if len(accounts) > 0 {
		header = "Active account: unknown"
	}
	if active != "" {
		header = "Active account: " + active
	}
	entries = append(entries, menuEntry{Title: header, Tag: TagHeader})
	if lastError != "" {
		entries = append(entries, menuEntry{Title: "  " + lastError, Tag: TagDetailsBase})
	}
	entries = append(entries, menuEntry{Separator: true})

	logins := make([]Account, 0, len(accounts))
	for _, view := range accounts {
		logins = append(logins, view.Account)
	}
	owners := loginOwners(logins, active)
	for i, account := range accounts {
		owner := owners[account.Account.identity()]
		duplicate := owner != "" && owner != account.Account.Name
		usageTitle := formatWeeklyUsage(account.Cache.Usage)
		if duplicate {
			usageTitle = "Same login as " + owner
		}
		entries = append(entries, menuEntry{
			Title:   fmt.Sprintf("%s    %s", account.Account.Name, usageTitle),
			Tag:     TagAccountBase + i,
			Enabled: true,
			Checked: account.Active,
		})
		if subtitle := buildAccountSubtitle(account.Account); subtitle != "" {
			entries = append(entries, menuEntry{Title: "  " + subtitle, Tag: TagDetailsBase + 4*i + 1})
		}
		if duplicate {
			entries = append(entries, menuEntry{
				Title: "  Reauthenticate " + account.Account.Name + " to restore its login",
				Tag:   TagDetailsBase + 4*i + 2,
			})
			continue
		}
		entries = append(entries, menuEntry{
			Title: "  " + formatWeeklyReset(account.Cache.Usage),
			Tag:   TagDetailsBase + 4*i + 2,
		})
		if subtitle := account.Cache.Warmup.subtitle(); subtitle != "" {
			entries = append(entries, menuEntry{Title: "  " + subtitle, Tag: TagDetailsBase + 4*i + 3})
		}
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
