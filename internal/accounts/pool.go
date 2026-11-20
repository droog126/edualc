package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,47}$`)

type Account struct {
	ID             string    `json:"id"`
	ConfigDir      string    `json:"config_dir"`
	LoggedIn       bool      `json:"logged_in"`
	Enabled        bool      `json:"enabled"`
	InFlight       int       `json:"in_flight"`
	TotalRequests  int64     `json:"total_requests"`
	LastUsedAt     time.Time `json:"last_used_at,omitempty"`
	CooldownUntil  time.Time     `json:"cooldown_until,omitempty"`
	LastError      string        `json:"last_error,omitempty"`
	Usage          *AccountUsage `json:"usage,omitempty"`
}

type persistedMeta struct {
	Disabled map[string]bool `json:"disabled"`
}

type Pool struct {
	mu       sync.Mutex
	baseDir  string
	accounts map[string]*Account
	disabled map[string]bool
}

func NewPool(baseDir string) (*Pool, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	p := &Pool{
		baseDir:  baseDir,
		accounts: make(map[string]*Account),
		disabled: make(map[string]bool),
	}
	p.loadMeta()
	if err := p.Refresh(); err != nil {
		return nil, err
	}
	// Ensure at least a "default" slot exists for out-of-the-box single-account setup.
	if len(p.accounts) == 0 {
		_, _ = p.Ensure("default")
	}
	return p, nil
}

func (p *Pool) metaPath() string {
	return filepath.Join(p.baseDir, ".pool_meta.json")
}

func (p *Pool) loadMeta() {
	b, err := os.ReadFile(p.metaPath())
	if err != nil {
		return
	}
	var m persistedMeta
	if json.Unmarshal(b, &m) == nil && m.Disabled != nil {
		p.disabled = m.Disabled
	}
}

func (p *Pool) saveMetaLocked() {
	b, _ := json.MarshalIndent(persistedMeta{Disabled: p.disabled}, "", "  ")
	_ = os.WriteFile(p.metaPath(), b, 0o600)
}

func (p *Pool) Ensure(id string) (*Account, error) {
	id = strings.TrimSpace(id)
	if !validNameRe.MatchString(id) {
		return nil, errors.New("账号名称仅允许字母、数字、点、下划线和短横线（1-48字符）")
	}
	dir := filepath.Join(p.baseDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.accounts[id]; ok {
		existing.LoggedIn = detectCredentials(dir)
		return cloneAccount(existing), nil
	}
	acc := &Account{
		ID:        id,
		ConfigDir: dir,
		LoggedIn:  detectCredentials(dir),
		Enabled:   !p.disabled[id],
	}
	p.accounts[id] = acc
	return cloneAccount(acc), nil
}

func (p *Pool) Refresh() error {
	entries, err := os.ReadDir(p.baseDir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	found := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		id := entry.Name()
		dir := filepath.Join(p.baseDir, id)
		found[id] = true
		if existing, ok := p.accounts[id]; ok {
			existing.LoggedIn = detectCredentials(dir)
			existing.Enabled = !p.disabled[id]
			if existing.Usage == nil {
				existing.Usage = loadCachedUsage(dir)
			}
		} else {
			p.accounts[id] = &Account{
				ID:        id,
				ConfigDir: dir,
				LoggedIn:  detectCredentials(dir),
				Enabled:   !p.disabled[id],
				Usage:     loadCachedUsage(dir),
			}
		}
	}
	for id := range p.accounts {
		if !found[id] {
			delete(p.accounts, id)
		}
	}
	return nil
}

func (p *Pool) List() []Account {
	_ = p.Refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, *cloneAccount(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Pool) SetEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[id]
	if !ok {
		return fmt.Errorf("未找到账号 %q", id)
	}
	acc.Enabled = enabled
	if enabled {
		delete(p.disabled, id)
	} else {
		p.disabled[id] = true
	}
	p.saveMetaLocked()
	return nil
}

// Acquire picks the best available logged-in account not in exclude and not cooling down.
func (p *Pool) Acquire(exclude map[string]bool) (*Account, error) {
	_ = p.Refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()

	var candidates []*Account
	for _, acc := range p.accounts {
		if !acc.Enabled || exclude[acc.ID] {
			continue
		}
		if !acc.LoggedIn {
			continue
		}
		if !acc.CooldownUntil.IsZero() && now.Before(acc.CooldownUntil) {
			continue
		}
		candidates = append(candidates, acc)
	}
	if len(candidates) == 0 {
		// Check if any account exists at all to provide an actionable error message.
		hasLoggedIn := false
		for _, acc := range p.accounts {
			if acc.LoggedIn && acc.Enabled {
				hasLoggedIn = true
				break
			}
		}
		if !hasLoggedIn {
			return nil, errors.New("当前没有已登录的 Claude 账号，请先运行 login 脚本在隔离环境中完成 claude auth login")
		}
		return nil, errors.New("所有已登录账号均处于限流冷却中或不可用，请稍后重试")
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].InFlight != candidates[j].InFlight {
			return candidates[i].InFlight < candidates[j].InFlight
		}
		if !candidates[i].LastUsedAt.Equal(candidates[j].LastUsedAt) {
			return candidates[i].LastUsedAt.Before(candidates[j].LastUsedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})

	chosen := candidates[0]
	chosen.InFlight++
	chosen.TotalRequests++
	chosen.LastUsedAt = now
	return cloneAccount(chosen), nil
}

func (p *Pool) Release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc, ok := p.accounts[id]; ok && acc.InFlight > 0 {
		acc.InFlight--
	}
}

func (p *Pool) MarkRateLimited(id string, cooldown time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc, ok := p.accounts[id]; ok {
		acc.CooldownUntil = time.Now().UTC().Add(cooldown)
		acc.LastError = reason
	}
}

func (p *Pool) ClearError(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc, ok := p.accounts[id]; ok {
		acc.LastError = ""
		acc.CooldownUntil = time.Time{}
	}
}

func (p *Pool) SetUsage(id string, u *AccountUsage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc, ok := p.accounts[id]; ok {
		acc.Usage = u
	}
}

func (p *Pool) Get(id string) (*Account, bool) {
	_ = p.Refresh()
	p.mu.Lock()
	defer p.mu.Unlock()
	acc, ok := p.accounts[id]
	if !ok {
		return nil, false
	}
	return cloneAccount(acc), true
}

// detectCredentials checks common Claude Code OAuth credential locations under CLAUDE_CONFIG_DIR.
func detectCredentials(dir string) bool {
	candidates := []string{
		filepath.Join(dir, ".credentials.json"),
		filepath.Join(dir, "credentials.json"),
		filepath.Join(dir, ".claude", ".credentials.json"),
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() && st.Size() > 10 {
			return true
		}
	}
	// Fallback: check .claude.json containing oauthAccount when stored inline.
	for _, path := range []string{
		filepath.Join(dir, ".claude.json"),
		filepath.Join(dir, ".claude", ".claude.json"),
	} {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), "oauthAccount") {
			return true
		}
	}
	return false
}

func cloneAccount(a *Account) *Account {
	cp := *a
	return &cp
}
