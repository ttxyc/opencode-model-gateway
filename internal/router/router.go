package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

const (
	KindModelScope = "modelscope"
	KindCline      = "cline"
	KindAtria      = "atria"
)

var (
	insufficientRe = regexp.MustCompile(`insufficient balance|魔粒不足|余额不足|quota exceeded|insufficient_quota`)
	keyFailureRe   = regexp.MustCompile(`insufficient|balance|magicube|credit|quota|rate.?limit|too many requests|free limit|limit reached|余额|欠费|魔粒|额度|限流`)
	retryAfterRe   = regexp.MustCompile(`(?i)try again in\s*(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?`)
)

type KeyState struct {
	Balance      *float64  `json:"balance,omitempty"`
	BalanceAt    time.Time `json:"balanceAt,omitempty"`
	BalanceError string    `json:"balanceError,omitempty"`
	CooldownTill time.Time `json:"cooldownUntil,omitempty"`
	LastError    string    `json:"lastError,omitempty"`
	UserID       string    `json:"-"`
}

type msState struct {
	Current      string
	Since        time.Time
	Exhausted    map[string]bool
	AllExhausted bool
	LastProbe    time.Time
	Initialized  bool
}

type Router struct {
	cfg    *config.Config
	log    *logx.Logger
	client *http.Client

	mu       sync.Mutex
	keys     map[string][]string
	keysMod  time.Time
	keyCheck time.Time
	states   map[string]map[string]*KeyState
	ms       msState
}

type ProviderStatus struct {
	ID             string      `json:"id"`
	Label          string      `json:"label"`
	Keys           []KeyStatus `json:"keys"`
	CurrentKey     string      `json:"currentKey,omitempty"`
	CurrentKeyHash string      `json:"currentKeyHash,omitempty"`
	AllExhausted   bool        `json:"allExhausted,omitempty"`
}

type KeyStatus struct {
	Key          string     `json:"key"`
	KeyHash      string     `json:"keyHash"`
	Balance      *float64   `json:"balance"`
	BalanceAt    *time.Time `json:"balanceAt,omitempty"`
	BalanceError string     `json:"balanceError,omitempty"`
	CooldownTill *time.Time `json:"cooldownUntil,omitempty"`
	Exhausted    bool       `json:"exhausted"`
	LastError    string     `json:"lastError,omitempty"`
}

func New(cfg *config.Config, log *logx.Logger) *Router {
	return &Router{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		keys:   map[string][]string{},
		states: map[string]map[string]*KeyState{},
		ms:     msState{Exhausted: map[string]bool{}},
	}
}

func Mask(key string) string {
	if len(key) <= 14 {
		return key
	}
	return key[:7] + "..." + key[len(key)-4:]
}

func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func (r *Router) Kind(provider string) string {
	if p, ok := r.cfg.Providers[provider]; ok {
		return p.Kind
	}
	return ""
}

func (r *Router) Keys(provider string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.loadKeysLocked(provider)...)
}

func (r *Router) loadKeysLocked(provider string) []string {
	now := time.Now()
	if now.Sub(r.keyCheck) < 5*time.Second {
		if keys, ok := r.keys[provider]; ok {
			return keys
		}
	}
	r.keyCheck = now
	if st, err := os.Stat(r.cfg.Paths.Keys); err == nil && st.ModTime().After(r.keysMod) {
		r.keysMod = st.ModTime()
		raw, err := os.ReadFile(r.cfg.Paths.Keys)
		if err == nil {
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) == nil {
				for id := range r.cfg.Providers {
					section, ok := doc[id]
					if !ok {
						r.keys[id] = nil
						continue
					}
					var s struct {
						Keys []string `json:"keys"`
						Key  string   `json:"key"`
					}
					_ = json.Unmarshal(section, &s)
					keys := s.Keys
					if len(keys) == 0 && s.Key != "" {
						keys = []string{s.Key}
					}
					out := keys[:0]
					for _, k := range keys {
						if strings.TrimSpace(k) != "" {
							out = append(out, k)
						}
					}
					r.keys[id] = out
				}
			}
		}
	}
	return r.keys[provider]
}

func (r *Router) stateLocked(provider, key string) *KeyState {
	m := r.states[provider]
	if m == nil {
		m = map[string]*KeyState{}
		r.states[provider] = m
	}
	s := m[key]
	if s == nil {
		s = &KeyState{}
		m[key] = s
	}
	return s
}

func (r *Router) balanceFresh(s *KeyState) bool {
	return !s.BalanceAt.IsZero() && time.Since(s.BalanceAt) < 60*time.Second
}

func (r *Router) RefreshAll(ctx context.Context) {
	for id := range r.cfg.Providers {
		r.RefreshProvider(ctx, id, true)
	}
}

func (r *Router) RefreshProvider(ctx context.Context, provider string, force bool) {
	keys := r.Keys(provider)
	if len(keys) == 0 || r.noBalance(provider) {
		return
	}
	r.mu.Lock()
	need := force
	if !need {
		for _, k := range keys {
			if !r.balanceFresh(r.stateLocked(provider, k)) {
				need = true
				break
			}
		}
	}
	r.mu.Unlock()
	if !need {
		return
	}
	type result struct {
		key     string
		balance *float64
		err     error
	}
	ch := make(chan result, len(keys))
	for _, key := range keys {
		go func(k string) {
			b, err := r.fetchBalance(ctx, provider, k)
			ch <- result{key: k, balance: b, err: err}
		}(key)
	}
	for range keys {
		res := <-ch
		r.mu.Lock()
		s := r.stateLocked(provider, res.key)
		s.BalanceAt = time.Now()
		if res.err != nil {
			s.BalanceError = res.err.Error()
			if strings.Contains(s.BalanceError, "HTTP 401") {
				s.UserID = ""
			}
		} else {
			s.Balance = res.balance
			s.BalanceError = ""
		}
		r.mu.Unlock()
	}
	r.writeState()
}

func (r *Router) noBalance(provider string) bool {
	return r.Kind(provider) != KindModelScope && r.Kind(provider) != KindCline
}

func (r *Router) fetchBalance(ctx context.Context, provider, key string) (*float64, error) {
	client := r.client
	if r.Kind(provider) == KindModelScope {
		url := os.Getenv("KEY_ROUTER_MS_BALANCE_URL")
		if url == "" {
			url = "https://modelscope.cn/openapi/v1/magicubes/balance"
		}
		var body struct {
			Data struct {
				Balance *float64 `json:"available_balance"`
			} `json:"data"`
		}
		if err := r.getJSON(ctx, client, url, key, &body); err != nil {
			return nil, err
		}
		if body.Data.Balance == nil {
			return nil, errors.New("unexpected balance payload")
		}
		return body.Data.Balance, nil
	}
	if r.Kind(provider) == KindCline {
		base := strings.TrimRight(os.Getenv("KEY_ROUTER_CLINE_API_BASE"), "/")
		if base == "" {
			base = "https://api.cline.bot/api/v1"
		}
		r.mu.Lock()
		s := r.stateLocked(provider, key)
		userID := s.UserID
		r.mu.Unlock()
		if userID == "" {
			var me struct {
				Data struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := r.getJSON(ctx, client, base+"/users/me", key, &me); err != nil {
				return nil, err
			}
			if me.Data.ID == "" {
				return nil, errors.New("unexpected cline user payload")
			}
			userID = me.Data.ID
			r.mu.Lock()
			r.stateLocked(provider, key).UserID = userID
			r.mu.Unlock()
		}
		var body struct {
			Data struct {
				Balance *float64 `json:"balance"`
			} `json:"data"`
		}
		if err := r.getJSON(ctx, client, base+"/users/"+userID+"/balance", key, &body); err != nil {
			return nil, err
		}
		if body.Data.Balance == nil {
			return nil, errors.New("unexpected balance payload")
		}
		return body.Data.Balance, nil
	}
	return nil, nil
}

func (r *Router) getJSON(ctx context.Context, client *http.Client, url, key string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (r *Router) Ensure(ctx context.Context, provider string) (string, error) {
	r.mu.Lock()
	keys := append([]string(nil), r.loadKeysLocked(provider)...)
	r.mu.Unlock()
	if len(keys) == 0 {
		return "", fmt.Errorf("provider %s has no keys", provider)
	}
	if r.Kind(provider) == KindModelScope {
		return r.ensureMS(ctx)
	}
	r.mu.Lock()
	needRefresh := false
	for _, k := range keys {
		if !r.balanceFresh(r.stateLocked(provider, k)) {
			needRefresh = true
			break
		}
	}
	r.mu.Unlock()
	if needRefresh {
		r.RefreshProvider(ctx, provider, true)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.Kind(provider) {
	case KindCline:
		return r.pickClineLocked(provider, keys, nil), nil
	case KindAtria:
		return r.pickPriorityLocked(provider, keys, nil), nil
	default:
		return r.pickPriorityLocked(provider, keys, nil), nil
	}
}

func (r *Router) Pick(ctx context.Context, provider string, tried map[string]bool) (string, error) {
	keys := r.Keys(provider)
	if len(keys) == 0 {
		return "", fmt.Errorf("provider %s has no keys", provider)
	}
	if r.Kind(provider) == KindModelScope {
		return r.pickMS(ctx, tried)
	}
	if r.Kind(provider) == KindCline {
		r.RefreshProvider(ctx, provider, false)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	available := make([]string, 0, len(keys))
	for _, k := range keys {
		if !tried[k] {
			available = append(available, k)
		}
	}
	if len(available) == 0 {
		return "", errors.New("no available key")
	}
	if r.Kind(provider) == KindCline {
		return r.pickClineLocked(provider, available, tried), nil
	}
	return r.pickPriorityLocked(provider, available, tried), nil
}

func (r *Router) ensureMS(ctx context.Context) (string, error) {
	r.mu.Lock()
	keys := append([]string(nil), r.loadKeysLocked(KindModelScope)...)
	r.mu.Unlock()
	if len(keys) == 0 {
		return "", errors.New("modelscope has no keys")
	}
	if !r.balanceKnown(KindModelScope, keys) {
		r.RefreshProvider(ctx, KindModelScope, true)
	}
	r.mu.Lock()
	if !r.ms.Initialized {
		r.ms.Initialized = true
	}
	current := r.ms.Current
	if current != "" && !contains(keys, current) {
		r.ms.Current = ""
		current = ""
	}
	if current != "" && !r.ms.Exhausted[Hash(current)] {
		s := r.stateLocked(KindModelScope, current)
		if s.Balance != nil && *s.Balance > 0 {
			r.mu.Unlock()
			return current, nil
		}
	}
	if r.ms.AllExhausted {
		r.ms.LastProbe = time.Now()
	}
	r.mu.Unlock()

	r.RefreshProvider(ctx, KindModelScope, true)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ms.AllExhausted {
		for _, k := range keys {
			s := r.stateLocked(KindModelScope, k)
			if s.Balance != nil && *s.Balance > 0 {
				delete(r.ms.Exhausted, Hash(k))
			}
		}
	}
	picked := r.bestMSLocked(keys, r.ms.Exhausted)
	if picked == "" {
		if r.ms.AllExhausted && time.Since(r.ms.LastProbe) < 5*time.Second {
			return "", errors.New("modelscope all keys exhausted")
		}
		r.ms.AllExhausted = true
		r.ms.Current = ""
		r.ms.LastProbe = time.Now()
		return "", errors.New("modelscope all keys exhausted")
	}
	r.ms.AllExhausted = false
	r.ms.Current = picked
	r.ms.Since = time.Now()
	r.writeStateLocked()
	return picked, nil
}

func (r *Router) pickMS(ctx context.Context, tried map[string]bool) (string, error) {
	key, err := r.ensureMS(ctx)
	if err != nil {
		return "", err
	}
	if !tried[key] {
		return key, nil
	}
	r.mu.Lock()
	keys := append([]string(nil), r.loadKeysLocked(KindModelScope)...)
	exclude := map[string]bool{}
	for k := range tried {
		exclude[Hash(k)] = true
	}
	picked := r.bestMSLocked(keys, exclude)
	r.mu.Unlock()
	if picked == "" {
		return "", errors.New("no available modelscope key")
	}
	return picked, nil
}

func (r *Router) balanceKnown(provider string, keys []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		if r.stateLocked(provider, k).Balance != nil {
			return true
		}
	}
	return false
}

func (r *Router) bestMSLocked(keys []string, exclude map[string]bool) string {
	var best string
	var bestBalance float64
	var unknown string
	for _, k := range keys {
		if exclude[Hash(k)] {
			continue
		}
		s := r.stateLocked(KindModelScope, k)
		if s.Balance == nil {
			if unknown == "" {
				unknown = k
			}
			continue
		}
		if *s.Balance > bestBalance {
			best, bestBalance = k, *s.Balance
		}
	}
	if best != "" {
		return best
	}
	return unknown
}

func (r *Router) pickClineLocked(provider string, keys []string, exclude map[string]bool) string {
	now := time.Now()
	var best, unknown, fallback, bestAny string
	bestBalance, bestAnyBalance := float64(0), float64(0)
	for _, k := range keys {
		if exclude != nil && exclude[k] {
			continue
		}
		s := r.stateLocked(provider, k)
		if s.Balance != nil && *s.Balance > bestAnyBalance {
			bestAny, bestAnyBalance = k, *s.Balance
		}
		if s.CooldownTill.After(now) {
			continue
		}
		if fallback == "" {
			fallback = k
		}
		if s.Balance == nil {
			if unknown == "" {
				unknown = k
			}
			continue
		}
		if *s.Balance > 0 && *s.Balance > bestBalance {
			best, bestBalance = k, *s.Balance
		}
	}
	if best != "" {
		return best
	}
	if unknown != "" {
		return unknown
	}
	if bestAny != "" {
		return bestAny
	}
	return fallback
}

func (r *Router) pickPriorityLocked(provider string, keys []string, exclude map[string]bool) string {
	now := time.Now()
	var fallback string
	var fallbackTill time.Time
	for _, k := range keys {
		if exclude != nil && exclude[k] {
			continue
		}
		s := r.stateLocked(provider, k)
		if !s.CooldownTill.After(now) {
			return k
		}
		if fallback == "" || s.CooldownTill.Before(fallbackTill) {
			fallback, fallbackTill = k, s.CooldownTill
		}
	}
	return fallback
}

func (r *Router) ReportFailure(ctx context.Context, provider, key string, status int, reason string) error {
	if r.Kind(provider) == KindModelScope {
		r.mu.Lock()
		s := r.stateLocked(provider, key)
		s.LastError = reason
		if status == http.StatusPaymentRequired || insufficientRe.MatchString(strings.ToLower(reason)) {
			zero := float64(0)
			s.Balance = &zero
			s.BalanceAt = time.Now()
			r.ms.Exhausted[Hash(key)] = true
			if r.ms.Current == key {
				r.ms.Current = ""
			}
		}
		r.mu.Unlock()
		r.RefreshProvider(ctx, provider, true)
		r.mu.Lock()
		keys := append([]string(nil), r.loadKeysLocked(provider)...)
		exclude := map[string]bool{}
		for k := range r.ms.Exhausted {
			exclude[k] = true
		}
		if status != http.StatusPaymentRequired && !insufficientRe.MatchString(strings.ToLower(reason)) {
			exclude[Hash(key)] = true
		}
		picked := r.bestMSLocked(keys, exclude)
		if picked == "" && status != http.StatusPaymentRequired && !insufficientRe.MatchString(strings.ToLower(reason)) {
			picked = r.bestMSLocked(keys, map[string]bool{Hash(key): true})
		}
		if picked == "" {
			if status != http.StatusPaymentRequired && !insufficientRe.MatchString(strings.ToLower(reason)) {
				r.ms.Current = key
				r.ms.AllExhausted = false
			} else {
				r.ms.AllExhausted = true
				r.ms.Current = ""
			}
		} else {
			r.ms.AllExhausted = false
			r.ms.Current = picked
			r.ms.Since = time.Now()
		}
		r.writeStateLocked()
		r.mu.Unlock()
		return nil
	}
	if r.Kind(provider) == KindCline || r.Kind(provider) == KindAtria {
		d := cooldownDuration(reason)
		r.mu.Lock()
		s := r.stateLocked(provider, key)
		s.CooldownTill = time.Now().Add(d)
		s.LastError = reason
		r.writeStateLocked()
		r.mu.Unlock()
	}
	return nil
}

func cooldownDuration(reason string) time.Duration {
	d := 5 * time.Minute
	m := retryAfterRe.FindStringSubmatch(reason)
	if len(m) == 4 {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		se, _ := strconv.Atoi(m[3])
		if h+mi+se > 0 {
			d = time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute + time.Duration(se)*time.Second
		}
	}
	if d < 5*time.Minute {
		d = 5 * time.Minute
	}
	if d > 24*time.Hour {
		d = 24 * time.Hour
	}
	return d
}

func IsKeyFailure(status int, body string) bool {
	if status == 401 || status == 402 || status == 403 || status == 429 {
		return true
	}
	return status >= 400 && status < 500 && keyFailureRe.MatchString(strings.ToLower(body))
}

func IsInsufficient(reason string) bool {
	return insufficientRe.MatchString(strings.ToLower(reason))
}

func (r *Router) KeyHashes(provider string) map[string]bool {
	out := map[string]bool{}
	for _, k := range r.Keys(provider) {
		out[Hash(k)] = true
	}
	return out
}

func (r *Router) ValidateKey(ctx context.Context, provider, key string) (*float64, error) {
	if _, ok := r.cfg.Providers[provider]; !ok {
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}
	if r.Kind(provider) == KindModelScope || r.Kind(provider) == KindCline {
		return r.fetchBalance(ctx, provider, key)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.Providers[provider].BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil, nil
}

func (r *Router) AddKey(provider, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cfg.Providers[provider]; !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}
	keys := append([]string(nil), r.loadKeysLocked(provider)...)
	for _, k := range keys {
		if k == key {
			return errors.New("key already exists")
		}
	}
	keys = append(keys, key)
	if err := r.writeKeysLocked(provider, keys); err != nil {
		return err
	}
	r.keys[provider] = keys
	r.keysMod = time.Time{}
	return nil
}

func (r *Router) DeleteKey(provider, keyHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := append([]string(nil), r.loadKeysLocked(provider)...)
	out := keys[:0]
	found := false
	for _, k := range keys {
		if Hash(k) == keyHash {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return errors.New("key not found")
	}
	if err := r.writeKeysLocked(provider, out); err != nil {
		return err
	}
	r.keys[provider] = out
	delete(r.states[provider], findKeyByHash(keys, keyHash))
	if r.ms.Current == findKeyByHash(keys, keyHash) {
		r.ms.Current = ""
	}
	r.keysMod = time.Time{}
	return nil
}

func findKeyByHash(keys []string, hash string) string {
	for _, k := range keys {
		if Hash(k) == hash {
			return k
		}
	}
	return ""
}

func (r *Router) writeKeysLocked(provider string, keys []string) error {
	path := r.cfg.Paths.Keys
	doc := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	section, _ := json.Marshal(map[string]any{"keys": keys, "label": r.cfg.Providers[provider].Label})
	doc[provider] = section
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		_ = err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

func (r *Router) Status() []ProviderStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked()
}

func (r *Router) statusLocked() []ProviderStatus {
	var out []ProviderStatus
	for id := range r.cfg.Providers {
		ps := ProviderStatus{ID: id, Label: r.cfg.Providers[id].Label}
		for _, k := range r.loadKeysLocked(id) {
			s := r.stateLocked(id, k)
			ks := KeyStatus{
				Key:          Mask(k),
				KeyHash:      Hash(k),
				Balance:      s.Balance,
				BalanceError: s.BalanceError,
				LastError:    s.LastError,
				Exhausted:    r.ms.Exhausted[Hash(k)],
			}
			if !s.BalanceAt.IsZero() {
				t := s.BalanceAt
				ks.BalanceAt = &t
			}
			if s.CooldownTill.After(time.Now()) {
				t := s.CooldownTill
				ks.CooldownTill = &t
			}
			ps.Keys = append(ps.Keys, ks)
		}
		if id == KindModelScope {
			if r.ms.Current != "" {
				ps.CurrentKey = Mask(r.ms.Current)
				ps.CurrentKeyHash = Hash(r.ms.Current)
			}
			ps.AllExhausted = r.ms.AllExhausted
		}
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Router) writeState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeStateLocked()
}

func (r *Router) writeStateLocked() {
	payload := map[string]any{
		"updatedAt": time.Now().UTC(),
		"providers": r.statusLocked(),
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(r.cfg.Paths.State), 0o755)
	tmp := r.cfg.Paths.State + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o600) == nil {
		_ = os.Remove(r.cfg.Paths.State)
		_ = os.Rename(tmp, r.cfg.Paths.State)
	}
}

func contains(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
