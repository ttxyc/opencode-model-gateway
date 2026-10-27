package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/router"
)

//go:embed web/admin.html
var webFS embed.FS

type UpdateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	URL       string `json:"url,omitempty"`
	Notes     string `json:"notes,omitempty"`
	Error     string `json:"error,omitempty"`
}

type UpdateManager interface {
	Check(ctx context.Context) (UpdateStatus, error)
	Apply(ctx context.Context) (UpdateStatus, error)
	Enabled() bool
	Cached() UpdateStatus
}

type Options struct {
	Version       string
	StartedAt     time.Time
	Update        UpdateManager
	ApplyUpdate   func(path string) error
	DisableWatch  bool
	AdminToken    string
	ClientToken   string
	MaxConcurrent int
}

type Server struct {
	cfg       *config.Config
	router    *router.Router
	log       *logx.Logger
	version   string
	started   time.Time
	update    UpdateManager
	apply     func(path string) error
	adminTok  string
	clientTok string

	active    atomic.Int64
	requests  atomic.Int64
	chatReqs  atomic.Int64
	errors    atomic.Int64
	retries   atomic.Int64
	startedAt string

	client    *http.Client
	transport *http.Transport
}

func New(cfg *config.Config, r *router.Router, log *logx.Logger, opts Options) *Server {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: time.Duration(cfg.Limits.HeadersTimeoutMS) * time.Millisecond,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
	}
	client := &http.Client{Transport: transport}
	max := opts.MaxConcurrent
	if max <= 0 {
		max = cfg.Limits.MaxConcurrent
	}
	s := &Server{
		cfg: cfg, router: r, log: log, version: opts.Version, started: opts.StartedAt,
		update: opts.Update, apply: opts.ApplyUpdate,
		adminTok: opts.AdminToken, clientTok: opts.ClientToken,
		client: client, transport: transport,
	}
	if s.adminTok == "" {
		s.adminTok = cfg.AdminToken
	}
	if s.clientTok == "" {
		s.clientTok = cfg.ClientToken
	}
	s.startedAt = s.started.UTC().Format(time.RFC3339)
	_ = max
	return s
}

func rid() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := rid()
	w.Header().Set("x-gateway-request-id", requestID)
	start := time.Now()
	status := 0
	defer func() {
		if v := recover(); v != nil {
			s.errors.Add(1)
			s.log.Crash("panic", map[string]any{"requestId": requestID, "value": fmt.Sprint(v), "stack": string(debug.Stack())})
			if status == 0 {
				writeError(w, http.StatusInternalServerError, "网关内部错误", "gateway_error", "panic", requestID)
			}
		}
		s.log.Info("http_done", map[string]any{
			"requestId": requestID, "method": r.Method, "path": r.URL.Path,
			"status": status, "ms": time.Since(start).Milliseconds(),
		})
	}()
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if path == "/healthz" || path == "/health" {
		status = http.StatusOK
		writeJSON(w, status, map[string]any{
			"ok": true, "ready": true, "version": s.version,
			"uptimeSec": int64(time.Since(s.started).Seconds()), "pid": os.Getpid(),
			"active": s.active.Load(), "modelscopeAllExhausted": s.msExhausted(),
		})
		return
	}
	if path == "/admin" || path == "/admin/" {
		status = http.StatusOK
		w.Header().Set("content-type", "text/html; charset=utf-8")
		b, _ := webFS.ReadFile("web/admin.html")
		_, _ = w.Write(b)
		return
	}
	if path == "/status" {
		status = s.handleStatus(w, r, requestID)
		return
	}
	if strings.HasPrefix(path, "/admin/api/") {
		status = s.handleAdmin(w, r, requestID, path)
		return
	}
	if strings.HasPrefix(path, "/p/") {
		status = s.handleProvider(w, r, requestID, path)
		return
	}
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		status = s.handleUnified(w, r, requestID, path)
		return
	}
	status = http.StatusNotFound
	writeError(w, status, "未知路径: "+path, "invalid_request_error", "not_found", requestID)
}

func (s *Server) msExhausted() bool {
	for _, p := range s.router.Status() {
		if p.ID == router.KindModelScope {
			return p.AllExhausted
		}
	}
	return false
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, requestID string) int {
	if !s.clientAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "网关令牌无效", "invalid_request_error", "invalid_api_key", requestID)
		return http.StatusUnauthorized
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": s.version, "startedAt": s.startedAt,
		"uptimeSec": int64(time.Since(s.started).Seconds()), "pid": os.Getpid(),
		"counters": map[string]any{
			"requests": s.requests.Load(), "chatRequests": s.chatReqs.Load(),
			"upstreamErrors": s.errors.Load(), "retries": s.retries.Load(), "active": s.active.Load(),
		},
		"providers": s.router.Status(),
	})
	return http.StatusOK
}

func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request, requestID, path string) int {
	rest := strings.TrimPrefix(path, "/p/")
	slash := strings.Index(rest, "/v1")
	if slash <= 0 {
		writeError(w, http.StatusNotFound, "路径必须为 /p/<provider>/v1/...", "invalid_request_error", "not_found", requestID)
		return http.StatusNotFound
	}
	providerID, _ := url.PathUnescape(rest[:slash])
	if _, ok := s.cfg.Providers[providerID]; !ok {
		writeError(w, http.StatusNotFound, "未知 provider: "+providerID, "invalid_request_error", "provider_not_found", requestID)
		return http.StatusNotFound
	}
	sub := rest[slash+3:]
	if sub == "" {
		sub = "/"
	}
	if !s.providerAuthorized(r, providerID) {
		writeError(w, http.StatusUnauthorized, "网关令牌无效", "invalid_request_error", "invalid_api_key", requestID)
		return http.StatusUnauthorized
	}
	if r.Method == http.MethodGet && sub == "/models" {
		return s.writeModels(w, providerID, false)
	}
	if r.Method == http.MethodPost && sub == "/chat/completions" {
		return s.handleChat(w, r, requestID, providerID, sub, true)
	}
	writeError(w, http.StatusNotFound, "不支持的接口: "+r.Method+" "+path, "invalid_request_error", "not_found", requestID)
	return http.StatusNotFound
}

func (s *Server) handleUnified(w http.ResponseWriter, r *http.Request, requestID, path string) int {
	if !s.clientAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "网关令牌无效", "invalid_request_error", "invalid_api_key", requestID)
		return http.StatusUnauthorized
	}
	sub := strings.TrimPrefix(path, "/v1")
	if sub == "" {
		sub = "/"
	}
	if r.Method == http.MethodGet && sub == "/models" {
		return s.writeModels(w, "", true)
	}
	if r.Method == http.MethodPost && sub == "/chat/completions" {
		return s.handleChat(w, r, requestID, "", sub, false)
	}
	writeError(w, http.StatusNotFound, "不支持的接口: "+r.Method+" "+path, "invalid_request_error", "not_found", requestID)
	return http.StatusNotFound
}

func (s *Server) writeModels(w http.ResponseWriter, provider string, unified bool) int {
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []item
	if unified {
		for _, m := range s.cfg.UnifiedModels() {
			data = append(data, item{ID: m["id"], Object: "model", OwnedBy: m["provider"]})
		}
	} else {
		models, _ := s.cfg.ProviderModels(provider)
		for _, m := range models {
			data = append(data, item{ID: m, Object: "model", OwnedBy: provider})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	return http.StatusOK
}

func (s *Server) clientAuthorized(r *http.Request) bool {
	tok := extractToken(r)
	return constantEqual(tok, s.clientTok)
}

func (s *Server) providerAuthorized(r *http.Request, provider string) bool {
	tok := extractToken(r)
	if constantEqual(tok, s.clientTok) {
		return true
	}
	for _, k := range s.router.Keys(provider) {
		if constantEqual(tok, k) {
			return true
		}
	}
	return false
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	if !isLoopback(r.RemoteAddr) {
		return false
	}
	tok := r.Header.Get("X-Admin-Token")
	if tok == "" {
		tok = extractToken(r)
	}
	return constantEqual(tok, s.adminTok)
}

func extractToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	for _, name := range []string{"x-api-key", "api-key", "x-gateway-token"} {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

func constantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, requestID, providerID, sub string, providerPath bool) int {
	if s.active.Load() >= int64(s.cfg.Limits.MaxConcurrent) {
		writeError(w, http.StatusTooManyRequests, "网关并发已满", "rate_limit_error", "gateway_busy", requestID)
		return http.StatusTooManyRequests
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	s.chatReqs.Add(1)
	start := time.Now()

	body, err := readBody(r.Body, s.cfg.Limits.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error(), "invalid_request_error", "body_too_large", requestID)
		return http.StatusRequestEntityTooLarge
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "请求体为空", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_request_error", "invalid_json", requestID)
		return http.StatusBadRequest
	}
	var model string
	_ = json.Unmarshal(payload["model"], &model)
	pid, upstreamModel, err := s.cfg.ResolveModel(providerID, model, !providerPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_model", requestID)
		return http.StatusBadRequest
	}
	if _, ok := s.cfg.Providers[pid]; !ok {
		writeError(w, http.StatusBadRequest, "provider 未接入: "+pid, "invalid_request_error", "provider_not_routed", requestID)
		return http.StatusBadRequest
	}
	payload["model"], _ = json.Marshal(upstreamModel)
	body, _ = json.Marshal(payload)
	provider := s.cfg.Providers[pid]
	upstreamURL := provider.BaseURL + sub + r.URL.RawQuery
	if r.URL.RawQuery != "" {
		upstreamURL = provider.BaseURL + sub + "?" + r.URL.RawQuery
	}
	tried := map[string]bool{}
	var lastStatus int
	var lastBody string
	attempts := 0
	retries := 0
	for attempts <= s.cfg.Limits.MaxRetries {
		key, err := s.router.Pick(r.Context(), pid, tried)
		if err != nil {
			if attempts == 0 || s.msExhaustedProvider(pid) {
				writeError(w, http.StatusServiceUnavailable, "provider 没有可用 key: "+err.Error(), "insufficient_quota", "all_keys_exhausted", requestID)
				return http.StatusServiceUnavailable
			}
			break
		}
		tried[key] = true
		attempts++
		ctx := r.Context()
		var cancel context.CancelFunc
		if s.cfg.Limits.TotalTimeoutMS > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(s.cfg.Limits.TotalTimeoutMS)*time.Millisecond)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			writeError(w, http.StatusInternalServerError, err.Error(), "gateway_error", "request_build_failed", requestID)
			return http.StatusInternalServerError
		}
		copyRequestHeaders(req, r)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if r.Context().Err() != nil {
				s.log.Warn("client_aborted", map[string]any{"requestId": requestID, "provider": pid})
				return 499
			}
			lastStatus, lastBody = 0, err.Error()
			s.log.Warn("upstream_error", map[string]any{"requestId": requestID, "provider": pid, "message": logx.Redact(err.Error(), 300)})
			if attempts <= s.cfg.Limits.MaxRetries && len(s.router.Keys(pid)) > len(tried) {
				retries++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			status := s.relay(w, r, resp, pid, requestID, attempts, providerPath)
			if cancel != nil {
				cancel()
			}
			s.requests.Add(1)
			s.log.Info("request_done", map[string]any{
				"requestId": requestID, "ok": true, "provider": pid, "model": upstreamModel,
				"keyHash": router.Hash(key), "attempts": attempts, "retries": retries,
				"stream": strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"),
				"ms":     time.Since(start).Milliseconds(),
			})
			return status
		}
		lastStatus = resp.StatusCode
		lastBody = readLimited(resp.Body, 64*1024)
		_ = resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if router.IsKeyFailure(resp.StatusCode, lastBody) {
			_ = s.router.ReportFailure(r.Context(), pid, key, resp.StatusCode, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, lastBody))
			s.log.Warn("key_failure", map[string]any{
				"requestId": requestID, "provider": pid, "keyHash": router.Hash(key),
				"status": resp.StatusCode, "message": logx.Redact(lastBody, 300),
			})
		}
		retryable := router.IsKeyFailure(resp.StatusCode, lastBody) || (resp.StatusCode >= 500 && resp.StatusCode <= 599)
		if !retryable || attempts > s.cfg.Limits.MaxRetries || len(tried) >= len(s.router.Keys(pid)) {
			break
		}
		retries++
		s.retries.Add(1)
		s.log.Warn("upstream_retry", map[string]any{"requestId": requestID, "provider": pid, "status": resp.StatusCode, "attempt": attempts + 1})
	}
	if s.msExhaustedProvider(pid) {
		writeError(w, http.StatusServiceUnavailable, "魔搭所有 key 魔粒均不足，已停止调用（充值或打卡到账后会自动恢复）", "insufficient_quota", "all_keys_exhausted", requestID)
		return http.StatusServiceUnavailable
	}
	if lastStatus == 0 {
		writeError(w, http.StatusBadGateway, "上游连接失败: "+lastBody, "upstream_error", "upstream_unreachable", requestID)
		return http.StatusBadGateway
	}
	if lastStatus == http.StatusUnauthorized || lastStatus == http.StatusForbidden {
		writeError(w, http.StatusBadGateway, "上游鉴权失败: "+lastBody, "upstream_error", "upstream_auth_failed", requestID)
		return http.StatusBadGateway
	}
	if lastStatus == http.StatusBadRequest || lastStatus == http.StatusNotFound || lastStatus == http.StatusUnprocessableEntity {
		writeError(w, lastStatus, lastBody, "invalid_request_error", "upstream_request_error", requestID)
		return lastStatus
	}
	writeError(w, lastStatus, lastBody, "upstream_error", "upstream_error", requestID)
	return lastStatus
}

func (s *Server) msExhaustedProvider(provider string) bool {
	if provider != router.KindModelScope {
		return false
	}
	for _, p := range s.router.Status() {
		if p.ID == router.KindModelScope {
			return p.AllExhausted
		}
	}
	return false
}

func copyRequestHeaders(dst, src *http.Request) {
	for _, name := range []string{"Accept", "User-Agent", "X-Title", "HTTP-Referer"} {
		if v := src.Header.Get(name); v != "" {
			dst.Header.Set(name, v)
		}
	}
	if dst.Header.Get("Accept") == "" {
		dst.Header.Set("Accept", "application/json")
	}
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request, resp *http.Response, provider, requestID string, attempts int, _ bool) int {
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("content-type", ct)
	w.Header().Set("x-gateway-request-id", requestID)
	w.Header().Set("x-gateway-provider", provider)
	w.Header().Set("x-gateway-attempts", strconv.Itoa(attempts))
	if strings.Contains(strings.ToLower(ct), "text/event-stream") {
		w.Header().Set("cache-control", "no-cache, no-transform")
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(resp.StatusCode)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		return resp.StatusCode
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error(), "upstream_error", "upstream_read_failed", requestID)
		return http.StatusBadGateway
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) == nil {
		if obj, ok := parsed.(map[string]any); ok {
			if _, hasChoices := obj["choices"]; !hasChoices {
				if data, ok := obj["data"].(map[string]any); ok {
					if _, ok := data["choices"]; ok {
						parsed = data
					}
				}
			}
			if out, err := json.Marshal(parsed); err == nil {
				raw = out
			}
		}
	}
	w.Header().Set("content-length", strconv.Itoa(len(raw)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
	return resp.StatusCode
}

func readBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errors.New("请求体超过上限")
	}
	return buf.Bytes(), nil
}

func readLimited(body io.ReadCloser, limit int64) string {
	defer body.Close()
	b, _ := io.ReadAll(io.LimitReader(body, limit))
	return strings.TrimSpace(string(b))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("content-length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, status int, message, typ, code, requestID string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": logx.Redact(message, 1500), "type": typ, "code": code, "request_id": requestID,
	}})
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, requestID, path string) int {
	if !s.adminAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "管理员令牌无效或非本机访问", "invalid_request_error", "invalid_admin_token", requestID)
		return http.StatusUnauthorized
	}
	switch path {
	case "/admin/api/status":
		out := map[string]any{
			"ok": true, "version": s.version, "providers": s.router.Status(),
			"counters": map[string]any{"requests": s.requests.Load(), "chatRequests": s.chatReqs.Load(), "active": s.active.Load()},
		}
		if s.update != nil {
			out["update"] = s.update.Cached()
		}
		writeJSON(w, http.StatusOK, out)
		return http.StatusOK
	case "/admin/api/keys":
		if r.Method == http.MethodPost {
			var body struct {
				Provider string `json:"provider"`
				Key      string `json:"key"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
				return http.StatusBadRequest
			}
			body.Provider = strings.TrimSpace(body.Provider)
			body.Key = strings.TrimSpace(body.Key)
			if body.Provider == "" || body.Key == "" {
				writeError(w, http.StatusBadRequest, "provider 和 key 不能为空", "invalid_request_error", "invalid_input", requestID)
				return http.StatusBadRequest
			}
			balance, err := s.router.ValidateKey(r.Context(), body.Provider, body.Key)
			if err != nil {
				writeError(w, http.StatusBadRequest, "key 校验失败: "+err.Error(), "invalid_request_error", "key_invalid", requestID)
				return http.StatusBadRequest
			}
			if err := s.router.AddKey(body.Provider, body.Key); err != nil {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "key_add_failed", requestID)
				return http.StatusBadRequest
			}
			s.router.RefreshProvider(r.Context(), body.Provider, true)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balance": balance, "providers": s.router.Status()})
			return http.StatusOK
		}
		if r.Method == http.MethodDelete {
			var body struct {
				Provider string `json:"provider"`
				KeyHash  string `json:"keyHash"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "JSON 无效", "invalid_request_error", "invalid_json", requestID)
				return http.StatusBadRequest
			}
			if err := s.router.DeleteKey(body.Provider, body.KeyHash); err != nil {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "key_delete_failed", requestID)
				return http.StatusBadRequest
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": s.router.Status()})
			return http.StatusOK
		}
	case "/admin/api/export-keys":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "只支持 GET", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		raw, err := os.ReadFile(s.cfg.Paths.Keys)
		if err != nil {
			if os.IsNotExist(err) {
				raw = []byte("{}\n")
			} else {
				writeError(w, http.StatusInternalServerError, "读取 key 配置失败: "+err.Error(), "gateway_error", "export_read_failed", requestID)
				return http.StatusInternalServerError
			}
		}
		w.Header().Set("content-type", "application/json; charset=utf-8")
		w.Header().Set("content-disposition", `attachment; filename="quota-line.json"`)
		w.Header().Set("cache-control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return http.StatusOK
	case "/admin/api/import-keys":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "只支持 POST", "invalid_request_error", "method_not_allowed", requestID)
			return http.StatusMethodNotAllowed
		}
		return s.handleImportKeys(w, r, requestID)
	case "/admin/api/update/check":
		if s.update == nil || !s.update.Enabled() {
			writeError(w, http.StatusBadRequest, "自动更新未启用", "invalid_request_error", "update_disabled", requestID)
			return http.StatusBadRequest
		}
		st, err := s.update.Check(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error(), "update_error", "update_check_failed", requestID)
			return http.StatusBadGateway
		}
		writeJSON(w, http.StatusOK, st)
		return http.StatusOK
	case "/admin/api/update/apply":
		if s.update == nil || !s.update.Enabled() {
			writeError(w, http.StatusBadRequest, "自动更新未启用", "invalid_request_error", "update_disabled", requestID)
			return http.StatusBadRequest
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_, err := s.update.Apply(ctx)
			if err != nil {
				s.log.Error("update_apply_failed", map[string]any{"error": logx.Redact(err.Error(), 500)})
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "更新已开始，服务可能会短暂重启"})
		return http.StatusAccepted
	}
	writeError(w, http.StatusNotFound, "未知管理接口", "invalid_request_error", "not_found", requestID)
	return http.StatusNotFound
}
