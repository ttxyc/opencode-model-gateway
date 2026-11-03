package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

type envelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type LoginStartResult struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
	Region  string `json:"region"`
}

type LoginPollResult struct {
	Status  string       `json:"status"` // pending | done
	Account *AccountView `json:"account,omitempty"`
}

type ModelInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	ContextWindow  int64  `json:"contextWindow,omitempty"`
	MaxTokens      int64  `json:"maxTokens,omitempty"`
	SupportsImages bool   `json:"supportsImages,omitempty"`
	Credits        string `json:"credits,omitempty"`
	Reasoning      any    `json:"reasoning,omitempty"`
}

func (m *Manager) baseHeaders() map[string]string {
	return map[string]string{
		"accept":        "application/json",
		"content-type":  "application/json",
		"user-agent":    UserAgent,
		"x-product":     "SaaS",
		"x-ide-type":    "CLI",
		"x-ide-name":    "CodeBuddy",
		"x-ide-version": ClientVersion,
	}
}

func noAccountHeaders() map[string]string {
	h := map[string]string{
		"X-No-Authorization":   "true",
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	return h
}

func addAccountHeaders(h map[string]string, a Account) {
	if a.UserID != "" {
		h["X-User-Id"] = a.UserID
	} else {
		h["X-No-User-Id"] = "true"
	}
	if a.EnterpriseID != "" {
		h["X-Enterprise-Id"] = a.EnterpriseID
		h["X-Tenant-Id"] = a.EnterpriseID
	} else {
		h["X-No-Enterprise-Id"] = "true"
	}
	if a.Domain != "" {
		h["X-Domain"] = a.Domain
	} else {
		h["X-No-Department-Info"] = "true"
	}
}

// ChatHeaders returns the identity headers for a WorkBuddy chat request.
func ChatHeaders(a Account, token string) map[string]string {
	h := map[string]string{
		"accept":        "text/event-stream",
		"content-type":  "application/json",
		"authorization": "Bearer " + token,
		"user-agent":    UserAgent,
		"x-product":     "SaaS",
		"x-ide-type":    "CLI",
		"x-ide-name":    "CodeBuddy",
		"x-ide-version": ClientVersion,
	}
	addAccountHeaders(h, a)
	return h
}

func (m *Manager) ChatBase(region, domain string) string { return m.chatBase(region, domain) }

func (m *Manager) doJSON(ctx context.Context, method, endpoint string, headers map[string]string, body []byte, timeout time.Duration) (int, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func envelopeError(status int, raw []byte) string {
	var env envelope
	if json.Unmarshal(raw, &env) == nil {
		msg := env.Msg
		if msg == "" {
			msg = env.Message
		}
		if msg != "" {
			return fmt.Sprintf("HTTP %d：%s", status, msg)
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d：%s", status, logx.Redact(s, 200))
}

// LoginStart creates an upstream device-auth state. The caller shows authUrl
// to the user; the gateway never opens a browser itself (it runs as a service).
func (m *Manager) LoginStart(ctx context.Context, region string) (LoginStartResult, error) {
	if region == "" {
		region = RegionCN
	}
	if region != RegionCN && region != RegionGlobal {
		return LoginStartResult{}, errors.New("region 只能是 cn 或 global")
	}
	base := m.pluginBase(region, "")
	h := m.baseHeaders()
	for k, v := range noAccountHeaders() {
		h[k] = v
	}
	u := base + "/auth/state?platform=" + url.QueryEscape(m.loginPlatform(region))
	status, raw, err := m.doJSON(ctx, http.MethodPost, u, h, []byte("{}"), 30*time.Second)
	if err != nil {
		return LoginStartResult{}, fmt.Errorf("无法连接 WorkBuddy 登录接口: %w", err)
	}
	if status < 200 || status >= 300 {
		return LoginStartResult{}, errors.New(envelopeError(status, raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 0 {
		return LoginStartResult{}, errors.New(envelopeError(status, raw))
	}
	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.State == "" || data.AuthURL == "" {
		return LoginStartResult{}, errors.New("登录接口没有返回 state/authUrl")
	}
	m.mu.Lock()
	m.cleanupPendingLocked(time.Now())
	m.pending[data.State] = &loginSession{
		State: data.State, Region: region, AuthURL: data.AuthURL,
		Base: base, Platform: m.loginPlatform(region), CreatedAt: time.Now(),
	}
	m.mu.Unlock()
	return LoginStartResult{State: data.State, AuthURL: data.AuthURL, Region: region}, nil
}

func (m *Manager) cleanupPendingLocked(now time.Time) {
	for state, p := range m.pending {
		if now.Sub(p.CreatedAt) > 15*time.Minute {
			delete(m.pending, state)
		}
	}
}

// LoginPoll checks whether the browser authorization completed. A non-fatal
// upstream error means "still pending", matching the official CLI flow.
func (m *Manager) LoginPoll(ctx context.Context, state string) (LoginPollResult, error) {
	m.mu.Lock()
	m.cleanupPendingLocked(time.Now())
	sess := m.pending[state]
	m.mu.Unlock()
	if sess == nil {
		return LoginPollResult{}, errors.New("登录会话不存在或已过期，请重新发起")
	}
	h := m.baseHeaders()
	for k, v := range noAccountHeaders() {
		h[k] = v
	}
	u := sess.Base + "/auth/token?state=" + url.QueryEscape(state)
	status, raw, err := m.doJSON(ctx, http.MethodGet, u, h, nil, 30*time.Second)
	if err != nil {
		return LoginPollResult{Status: "pending"}, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		m.forgetPending(state)
		return LoginPollResult{}, errors.New(envelopeError(status, raw))
	}
	if status < 200 || status >= 300 {
		return LoginPollResult{Status: "pending"}, nil
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || env.Code != 0 || len(env.Data) == 0 {
		return LoginPollResult{Status: "pending"}, nil
	}
	var auth struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		Domain           string `json:"domain"`
	}
	if json.Unmarshal(env.Data, &auth) != nil {
		return LoginPollResult{Status: "pending"}, nil
	}
	if auth.AccessToken == "" {
		return LoginPollResult{Status: "pending"}, nil
	}
	if auth.RefreshToken == "" {
		m.forgetPending(state)
		return LoginPollResult{}, errors.New("登录接口没有返回 refreshToken，请重试")
	}
	acc, err := m.fetchLoginAccount(ctx, sess, auth.AccessToken)
	if err != nil {
		return LoginPollResult{}, err
	}
	now := time.Now().UnixMilli()
	acc.AccessToken = auth.AccessToken
	acc.RefreshToken = auth.RefreshToken
	if auth.ExpiresIn > 0 {
		acc.ExpiresAt = now + auth.ExpiresIn*1000
	}
	if auth.RefreshExpiresIn > 0 {
		acc.RefreshExpiresAt = now + auth.RefreshExpiresIn*1000
	}
	if acc.Domain == "" {
		acc.Domain = auth.Domain
	}
	if acc.Domain == "" {
		acc.Domain = defaultDomain(sess.Region)
	}
	acc.Region = ClassifyDomain(acc.Domain)
	acc.Status = "ok"
	acc.UpdatedAt = now
	if acc.ID == "" {
		acc.ID = accountID(acc.UserID, acc.Email, auth.RefreshToken)
	}
	if _, err := m.Upsert(acc); err != nil {
		return LoginPollResult{}, fmt.Errorf("保存账号失败: %w", err)
	}
	m.forgetPending(state)
	v := viewOf(acc)
	return LoginPollResult{Status: "done", Account: &v}, nil
}

func (m *Manager) forgetPending(state string) {
	m.mu.Lock()
	delete(m.pending, state)
	m.mu.Unlock()
}

func (m *Manager) fetchLoginAccount(ctx context.Context, sess *loginSession, accessToken string) (Account, error) {
	h := m.baseHeaders()
	h["authorization"] = "Bearer " + accessToken
	h["X-No-User-Id"] = "true"
	h["X-No-Enterprise-Id"] = "true"
	h["X-No-Department-Info"] = "true"
	u := sess.Base + "/login/account?state=" + url.QueryEscape(sess.State)
	status, raw, err := m.doJSON(ctx, http.MethodGet, u, h, nil, 30*time.Second)
	if err != nil {
		return Account{}, fmt.Errorf("获取 WorkBuddy 账号失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return Account{}, errors.New(envelopeError(status, raw))
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || env.Code != 0 {
		return Account{}, errors.New(envelopeError(status, raw))
	}
	var doc map[string]any
	if err := json.Unmarshal(env.Data, &doc); err != nil {
		return Account{}, errors.New("账号接口返回格式无法解析")
	}
	return accountFromMap(doc, sess.Region), nil
}

func accountFromMap(doc map[string]any, region string) Account {
	sources := []map[string]any{doc}
	for _, key := range []string{"account", "user", "userInfo", "profile", "data"} {
		if v, ok := doc[key].(map[string]any); ok {
			sources = append(sources, v)
		}
	}
	read := func(keys ...string) string {
		for _, src := range sources {
			for _, k := range keys {
				if v, ok := src[k]; ok {
					switch t := v.(type) {
					case string:
						if strings.TrimSpace(t) != "" {
							return strings.TrimSpace(t)
						}
					case float64:
						return fmt.Sprintf("%.0f", t)
					}
				}
			}
		}
		return ""
	}
	userID := read("userId", "uid", "user_id", "id")
	email := read("email", "mail", "emailAddress")
	label := read("displayName", "name", "nickname", "username", "accountName")
	if label == "" {
		label = read("uin", "phoneNumber", "phone", "mobile")
	}
	if label == "" {
		label = email
	}
	acc := Account{
		Label: label, Region: region,
		UserID: userID, EnterpriseID: read("enterpriseId", "tenantId", "enterprise_id", "tenant_id"),
		Uin: read("uin", "phoneNumber", "phone", "mobile"), Email: email, Type: read("type", "accountType"),
		Status: "ok",
	}
	if userID != "" || email != "" {
		acc.ID = accountID(userID, email, "")
	}
	return acc
}

// EnsureAccess returns a valid access token, refreshing it when it is close to
// expiry. Refreshes are single-flight per account.
func (m *Manager) EnsureAccess(ctx context.Context, acc *Account) (string, error) {
	if acc == nil {
		return "", errors.New("账号为空")
	}
	if !acc.expired(time.Now(), 2*time.Minute) {
		return acc.AccessToken, nil
	}
	if acc.RefreshToken == "" {
		return "", errors.New("账号缺少 refreshToken，请重新登录")
	}

	m.refreshMu.Lock()
	if call, ok := m.refresh[acc.ID]; ok {
		m.refreshMu.Unlock()
		<-call.done
		if call.err != nil {
			return "", call.err
		}
		if call.account.AccessToken != "" {
			*acc = call.account
		}
		return acc.AccessToken, nil
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refresh[acc.ID] = call
	m.refreshMu.Unlock()

	defer func() {
		m.refreshMu.Lock()
		delete(m.refresh, acc.ID)
		m.refreshMu.Unlock()
		close(call.done)
	}()

	updated, err := m.refreshAccount(ctx, *acc)
	if err != nil {
		if acc.AccessToken != "" && !acc.expired(time.Now(), 0) {
			m.logWarn("workbuddy_refresh_soft_fail", map[string]any{
				"account": acc.ID, "error": logx.Redact(err.Error(), 300),
			})
			call.account = *acc
			return call.account.AccessToken, nil
		}
		call.err = err
		return "", err
	}
	*acc = updated
	call.account = updated
	return call.account.AccessToken, nil
}

func (m *Manager) refreshAccount(ctx context.Context, acc Account) (Account, error) {
	base := m.pluginBase(acc.Region, acc.Domain)
	h := m.baseHeaders()
	h["X-Refresh-Token"] = acc.RefreshToken
	h["X-Auth-Refresh-Source"] = "plugin"
	addAccountHeaders(h, acc)
	u := base + "/auth/token/refresh"
	status, raw, err := m.doJSON(ctx, http.MethodPost, u, h, []byte("{}"), 30*time.Second)
	if err != nil {
		return acc, fmt.Errorf("刷新 WorkBuddy 令牌失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return acc, errors.New(envelopeError(status, raw))
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || env.Code != 0 {
		return acc, errors.New(envelopeError(status, raw))
	}
	var data struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresIn        int64  `json:"expiresIn"`
		RefreshExpiresIn int64  `json:"refreshExpiresIn"`
		Domain           string `json:"domain"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return acc, errors.New("刷新接口返回格式无法解析")
	}
	if data.AccessToken == "" {
		return acc, errors.New("刷新接口没有返回 accessToken，请重新登录")
	}
	now := time.Now().UnixMilli()
	acc.AccessToken = data.AccessToken
	if data.RefreshToken != "" {
		acc.RefreshToken = data.RefreshToken
	}
	if data.ExpiresIn > 0 {
		acc.ExpiresAt = now + data.ExpiresIn*1000
	}
	if data.RefreshExpiresIn > 0 {
		acc.RefreshExpiresAt = now + data.RefreshExpiresIn*1000
	}
	if data.Domain != "" {
		acc.Domain = data.Domain
		acc.Region = ClassifyDomain(data.Domain)
	}
	acc.Status = "ok"
	acc.LastError = ""
	acc.UpdatedAt = now

	m.mu.Lock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].ID == acc.ID {
			m.store.Accounts[i] = acc
			break
		}
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	return acc, nil
}

// RefreshNow forces a refresh for the admin "测试/刷新" action.
func (m *Manager) RefreshNow(ctx context.Context, id string) (AccountView, error) {
	acc, ok := m.Find(id)
	if !ok {
		return AccountView{}, errors.New("账号不存在")
	}
	if acc.RefreshToken == "" {
		return AccountView{}, errors.New("账号缺少 refreshToken")
	}
	updated, err := m.refreshAccount(ctx, acc)
	if err != nil {
		return AccountView{}, err
	}
	return viewOf(updated), nil
}

// FetchModels reads /v3/config for one account. It is display-only: the
// gateway's /v1/models list still comes from gateway.json.
func (m *Manager) FetchModels(ctx context.Context, id string) ([]ModelInfo, error) {
	acc, ok := m.Find(id)
	if !ok {
		return nil, errors.New("账号不存在")
	}
	token, err := m.EnsureAccess(ctx, &acc)
	if err != nil {
		return nil, err
	}
	h := ChatHeaders(acc, token)
	h["accept"] = "application/json"
	delete(h, "content-type")
	u := m.chatBase(acc.Region, acc.Domain) + "/v3/config"
	status, raw, err := m.doJSON(ctx, http.MethodGet, u, h, nil, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("获取模型目录失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, errors.New(envelopeError(status, raw))
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Code != 0 {
		return nil, errors.New(envelopeError(status, raw))
	}
	var doc struct {
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
		Agent struct {
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"agent"`
		Models []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			MaxInputTokens  int64  `json:"maxInputTokens"`
			MaxAllowedSize  int64  `json:"maxAllowedSize"`
			MaxOutputTokens int64  `json:"maxOutputTokens"`
			SupportsImages  bool   `json:"supportsImages"`
			Credits         string `json:"credits"`
			Reasoning       any    `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(env.Data, &doc); err != nil {
		return nil, errors.New("模型目录格式无法解析")
	}
	agents := doc.Agents
	if len(agents) == 0 {
		agents = doc.Agent.Agents
	}
	allowed := []string{}
	for _, a := range agents {
		if a.Name == "cli" {
			allowed = a.Models
			break
		}
	}
	byID := map[string]int{}
	for i, m := range doc.Models {
		byID[m.ID] = i
	}
	out := make([]ModelInfo, 0, len(allowed))
	for _, id := range allowed {
		i, ok := byID[id]
		if !ok {
			continue
		}
		raw := doc.Models[i]
		ctxWin := raw.MaxInputTokens
		if ctxWin == 0 {
			ctxWin = raw.MaxAllowedSize
		}
		out = append(out, ModelInfo{
			ID: id, Name: raw.Name, ContextWindow: ctxWin, MaxTokens: raw.MaxOutputTokens,
			SupportsImages: raw.SupportsImages, Credits: raw.Credits, Reasoning: raw.Reasoning,
		})
	}
	return out, nil
}
