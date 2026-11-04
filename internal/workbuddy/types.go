// Package workbuddy implements the WorkBuddy (CodeBuddy) upstream used by the
// gateway. It is deliberately independent from the generic provider key pool:
// WorkBuddy uses an access/refresh token pair instead of a static API key, and
// it needs request shaping that no other provider does.
package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

const (
	// KindCN and KindGlobal are the provider `kind` values in gateway.json.
	KindCN     = "workbuddy"
	KindGlobal = "workbuddy-global"

	RegionCN     = "cn"
	RegionGlobal = "global"

	// ClientVersion is the CodeBuddy CLI version reported in the identity
	// headers. Bump it together with the User-Agent when upstream drifts.
	ClientVersion = "2.177.1"
	UserAgent     = "CLI/" + ClientVersion + " CodeBuddy/" + ClientVersion
)

// Account is one WorkBuddy login session. Tokens never leave the gateway.
type Account struct {
	ID               string `json:"id"`
	Label            string `json:"label,omitempty"`
	Region           string `json:"region"`
	Domain           string `json:"domain,omitempty"`
	UserID           string `json:"userId,omitempty"`
	EnterpriseID     string `json:"enterpriseId,omitempty"`
	Uin              string `json:"uin,omitempty"`
	Email            string `json:"email,omitempty"`
	Type             string `json:"type,omitempty"`
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"`
	// Status is ok, invalid (needs re-login) or exhausted (credits/cooldown).
	Status        string `json:"status,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
	LastUsedAt    int64  `json:"lastUsedAt,omitempty"`
}

func (a Account) clone() Account { return a }

func (a Account) expired(now time.Time, margin time.Duration) bool {
	if a.AccessToken == "" {
		return true
	}
	if a.ExpiresAt <= 0 {
		return false
	}
	return a.ExpiresAt <= now.Add(margin).UnixMilli()
}

// AccountView is the token-free shape exposed by the admin API.
type AccountView struct {
	ID               string `json:"id"`
	Label            string `json:"label,omitempty"`
	Region           string `json:"region"`
	Domain           string `json:"domain,omitempty"`
	UserID           string `json:"userId,omitempty"`
	Uin              string `json:"uin,omitempty"`
	Type             string `json:"type,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"`
	Status           string `json:"status,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	CooldownUntil    int64  `json:"cooldownUntil,omitempty"`
	UpdatedAt        int64  `json:"updatedAt,omitempty"`
	LastUsedAt       int64  `json:"lastUsedAt,omitempty"`
}

func viewOf(a Account) AccountView {
	return AccountView{
		ID: a.ID, Label: a.Label, Region: a.Region, Domain: a.Domain,
		UserID: a.UserID, Uin: a.Uin, Type: a.Type,
		ExpiresAt: a.ExpiresAt, RefreshExpiresAt: a.RefreshExpiresAt,
		Status: a.Status, LastError: a.LastError, CooldownUntil: a.CooldownUntil,
		UpdatedAt: a.UpdatedAt, LastUsedAt: a.LastUsedAt,
	}
}

type Store struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Endpoints is overridable so tests can point at an httptest server. The
// defaults are the production gateways observed working on 2026-11-03.
type Endpoints struct {
	CNPluginBase               string
	CNChatBase                 string
	GlobalPluginBase           string
	GlobalChatBase             string
	GlobalCodeBuddyPluginBase  string
	GlobalCodeBuddyChatBase    string
	PlatformCN                 string
	PlatformGlobal             string
	CNBillingBase              string
	GlobalBillingBase          string
	GlobalCodeBuddyBillingBase string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		CNPluginBase:               "https://copilot.tencent.com/v2/plugin",
		CNChatBase:                 "https://copilot.tencent.com",
		GlobalPluginBase:           "https://www.workbuddy.ai/v2/plugin",
		GlobalChatBase:             "https://www.workbuddy.ai",
		GlobalCodeBuddyPluginBase:  "https://www.codebuddy.ai/v2/plugin",
		GlobalCodeBuddyChatBase:    "https://www.codebuddy.ai",
		PlatformCN:                 "CLI",
		PlatformGlobal:             "workbuddy",
		CNBillingBase:              "https://www.codebuddy.cn",
		GlobalBillingBase:          "https://www.workbuddy.ai",
		GlobalCodeBuddyBillingBase: "https://www.codebuddy.ai",
	}
}

type loginSession struct {
	State     string
	Region    string
	AuthURL   string
	Base      string
	Platform  string
	CreatedAt time.Time
}

type refreshCall struct {
	done    chan struct{}
	account Account
	err     error
}

// Manager owns the credential store and the account pool.
type Manager struct {
	mu    sync.RWMutex
	path  string
	store Store
	log   *logx.Logger

	client *http.Client
	ep     Endpoints

	rr      map[string]int
	pending map[string]*loginSession

	refreshMu sync.Mutex
	refresh   map[string]*refreshCall

	creditsCache map[string]creditsCacheEntry
}

type creditsCacheEntry struct {
	at     time.Time
	result CreditsResult
}

// NewManager loads the state file. A missing or corrupt file starts empty; the
// gateway must not fail to boot because WorkBuddy is not configured yet.
func NewManager(stateFile string, log *logx.Logger) *Manager {
	m := &Manager{
		path:         stateFile,
		log:          log,
		ep:           DefaultEndpoints(),
		rr:           map[string]int{},
		pending:      map[string]*loginSession{},
		refresh:      map[string]*refreshCall{},
		creditsCache: map[string]creditsCacheEntry{},
	}
	m.client = &http.Client{Transport: &http.Transport{
		Proxy:                 nil, // WorkBuddy CN/global must go direct; the gateway service has HTTPS_PROXY set.
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxIdleConns:          60,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
	}}
	m.load()
	return m
}

func (m *Manager) SetEndpoints(ep Endpoints) { m.ep = ep }

func (m *Manager) logInfo(msg string, kv map[string]any) {
	if m.log != nil {
		m.log.Info(msg, kv)
	}
}

func (m *Manager) logWarn(msg string, kv map[string]any) {
	if m.log != nil {
		m.log.Warn(msg, kv)
	}
}

func (m *Manager) load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := os.ReadFile(m.path)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logWarn("workbuddy_state_read_failed", map[string]any{"error": logx.Redact(err.Error(), 300)})
		}
		m.store.Version = 1
		return
	}
	var st Store
	if err := json.Unmarshal(raw, &st); err != nil {
		m.logWarn("workbuddy_state_parse_failed", map[string]any{"error": logx.Redact(err.Error(), 300)})
		m.store.Version = 1
		return
	}
	if st.Version == 0 {
		st.Version = 1
	}
	m.store = st
}

func (m *Manager) saveLocked() error {
	if m.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		// Windows can refuse a rename over an existing file; retry after removing.
		_ = os.Remove(m.path)
		if err2 := os.Rename(tmp, m.path); err2 != nil {
			return err
		}
	}
	return nil
}

// ClassifyDomain maps a credential domain to a region.
func ClassifyDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") ||
		d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai") {
		return RegionGlobal
	}
	return RegionCN
}

func RegionForKind(kind string) string {
	if kind == KindGlobal {
		return RegionGlobal
	}
	return RegionCN
}

func IsKind(kind string) bool { return kind == KindCN || kind == KindGlobal }

func isCodeBuddyDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai")
}

func (m *Manager) pluginBase(region, domain string) string {
	if region == RegionGlobal {
		if isCodeBuddyDomain(domain) {
			return m.ep.GlobalCodeBuddyPluginBase
		}
		return m.ep.GlobalPluginBase
	}
	return m.ep.CNPluginBase
}

func (m *Manager) chatBase(region, domain string) string {
	if region == RegionGlobal {
		if isCodeBuddyDomain(domain) {
			return m.ep.GlobalCodeBuddyChatBase
		}
		return m.ep.GlobalChatBase
	}
	return m.ep.CNChatBase
}

func (m *Manager) loginPlatform(region string) string {
	if region == RegionGlobal {
		return m.ep.PlatformGlobal
	}
	return m.ep.PlatformCN
}

func defaultDomain(region string) string {
	if region == RegionGlobal {
		return "workbuddy.ai"
	}
	return "www.codebuddy.cn"
}

func accountID(userID, email, refreshToken string) string {
	if userID != "" {
		return "user:" + userID
	}
	if email != "" {
		return "email:" + email
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return "token:" + hex.EncodeToString(sum[:8])
}

// Upsert inserts or replaces an account by ID and marks it healthy.
func (m *Manager) Upsert(acc Account) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UnixMilli()
	acc.UpdatedAt = now
	acc.Status = "ok"
	acc.LastError = ""
	acc.CooldownUntil = 0
	for i := range m.store.Accounts {
		if m.store.Accounts[i].ID == acc.ID {
			m.creditsCache = map[string]creditsCacheEntry{}
			if acc.LastUsedAt == 0 {
				acc.LastUsedAt = m.store.Accounts[i].LastUsedAt
			}
			m.store.Accounts[i] = acc
			return false, m.saveLocked()
		}
	}
	m.store.Accounts = append(m.store.Accounts, acc)
	m.creditsCache = map[string]creditsCacheEntry{}
	return true, m.saveLocked()
}

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.store.Accounts[:0]
	for _, a := range m.store.Accounts {
		if a.ID != id {
			out = append(out, a)
		}
	}
	m.store.Accounts = out
	m.creditsCache = map[string]creditsCacheEntry{}
	return m.saveLocked()
}

// Reset clears invalid/exhausted state so an account becomes a candidate again.
func (m *Manager) Reset(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].ID == id {
			m.creditsCache = map[string]creditsCacheEntry{}
			m.store.Accounts[i].Status = "ok"
			m.store.Accounts[i].LastError = ""
			m.store.Accounts[i].CooldownUntil = 0
			m.store.Accounts[i].UpdatedAt = time.Now().UnixMilli()
			return m.saveLocked()
		}
	}
	return fmt.Errorf("账号不存在")
}

func (m *Manager) Find(id string) (Account, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.store.Accounts {
		if a.ID == id {
			return a.clone(), true
		}
	}
	return Account{}, false
}

func (m *Manager) Count(region string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, a := range m.store.Accounts {
		if a.Region == region {
			n++
		}
	}
	return n
}

// Accounts returns copies of every account in one region.
func (m *Manager) Accounts(region string) []Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Account, 0, len(m.store.Accounts))
	for _, a := range m.store.Accounts {
		if a.Region == region {
			out = append(out, a.clone())
		}
	}
	return out
}

// Pick returns the next eligible account in a round-robin order. The returned
// value is a copy; callers must not rely on later mutation being visible.
func (m *Manager) Pick(region string, exclude map[string]bool) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UnixMilli()
	idx := make([]int, 0, len(m.store.Accounts))
	for i, a := range m.store.Accounts {
		if a.Region != region {
			continue
		}
		if exclude != nil && exclude[a.ID] {
			continue
		}
		if a.Status == "invalid" {
			continue
		}
		if a.Status == "exhausted" && (a.CooldownUntil == 0 || a.CooldownUntil > now) {
			continue
		}
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return nil
	}
	start := m.rr[region] % len(idx)
	pick := idx[start]
	m.rr[region] = (start + 1) % len(idx)
	m.store.Accounts[pick].LastUsedAt = now
	a := m.store.Accounts[pick].clone()
	return &a
}

// Report records an account-level outcome. CooldownMS applies to exhausted
// accounts (credits/rate reset) and is ignored for other statuses.
func (m *Manager) Report(id, status, message string, cooldownMS int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].ID != id {
			continue
		}
		if status != "" {
			m.store.Accounts[i].Status = status
		}
		m.store.Accounts[i].LastError = message
		m.store.Accounts[i].UpdatedAt = time.Now().UnixMilli()
		if cooldownMS > 0 {
			m.store.Accounts[i].CooldownUntil = time.Now().Add(time.Duration(cooldownMS) * time.Millisecond).UnixMilli()
		}
		_ = m.saveLocked()
		return
	}
}

// Snapshot returns every account without tokens, sorted by region then label.
func (m *Manager) Snapshot() []AccountView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AccountView, 0, len(m.store.Accounts))
	for _, a := range m.store.Accounts {
		out = append(out, viewOf(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Region != out[j].Region {
			return out[i].Region < out[j].Region
		}
		return out[i].Label < out[j].Label
	})
	return out
}

type PendingView struct {
	State     string `json:"state"`
	Region    string `json:"region"`
	AuthURL   string `json:"authUrl,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func (m *Manager) Pending() []PendingView {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]PendingView, 0, len(m.pending))
	for state, p := range m.pending {
		if now.Sub(p.CreatedAt) > 15*time.Minute {
			delete(m.pending, state)
			continue
		}
		out = append(out, PendingView{State: state, Region: p.Region, AuthURL: p.AuthURL, CreatedAt: p.CreatedAt.UnixMilli()})
	}
	return out
}
