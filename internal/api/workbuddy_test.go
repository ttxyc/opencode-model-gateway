package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/router"
	"github.com/ttxyc/opencode-model-gateway/internal/workbuddy"
)

func newWorkBuddyHarness(t *testing.T, rejectFirst bool) (*httptest.Server, *config.Config, *Server, *int32, *[]string, *int32) {
	t.Helper()
	var calls int32
	var refreshCalls int32
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			if got := r.Header.Get("X-Refresh-Token"); got != "wb-refresh" {
				t.Errorf("refresh token header = %q", got)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"wb-access-2","refreshToken":"wb-refresh-2","expiresIn":3600}}`)
			return
		}
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer wb-access" && got != "Bearer wb-access-2" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-Product"); got != "SaaS" {
			t.Errorf("x-product = %q", got)
		}
		if got := r.Header.Get("X-User-Id"); got != "u1" {
			t.Errorf("x-user-id = %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		n := atomic.AddInt32(&calls, 1)
		if rejectFirst && n == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11128,"msg":"unapproved channel"}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		ClientToken: "client-token-123456",
		AdminToken:  "admin-token-123456",
		Providers: map[string]*config.Provider{
			workbuddy.KindCN: {
				Label: "WorkBuddy 中国区", BaseURL: "https://copilot.tencent.com/v2",
				Models: []string{"deepseek-v4.1-flash"}, Kind: workbuddy.KindCN,
			},
		},
		Limits: config.Limits{
			MaxBodyBytes: 1 << 20, MaxConcurrent: 8,
			HeadersTimeoutMS: 5000, TotalTimeoutMS: 20000, MaxRetries: 1,
		},
		Log:   config.LogConfig{Dir: filepath.Join(dir, "logs"), MaxBytes: 1 << 20, MaxFiles: 2},
		Paths: config.Paths{State: filepath.Join(dir, "state", "key-router.json")},
	}
	cfg.Normalize()
	log, err := logx.New(cfg.Log.Dir, cfg.Log.MaxBytes, cfg.Log.MaxFiles)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, log)
	s := New(cfg, rt, log, Options{Version: "test", StartedAt: time.Now()})
	s.wb.SetEndpoints(workbuddy.Endpoints{
		CNPluginBase: up.URL + "/v2/plugin", CNChatBase: up.URL,
		GlobalPluginBase: up.URL + "/v2/plugin", GlobalChatBase: up.URL,
		GlobalCodeBuddyPluginBase: up.URL + "/v2/plugin", GlobalCodeBuddyChatBase: up.URL,
		PlatformCN: "CLI", PlatformGlobal: "workbuddy",
	})
	if _, err := s.wb.Upsert(workbuddy.Account{
		ID: "user:u1", Label: "tester", Region: workbuddy.RegionCN, Domain: "www.codebuddy.cn",
		UserID: "u1", AccessToken: "wb-access", RefreshToken: "wb-refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, cfg, s, &calls, &bodies, &refreshCalls
}

func TestWorkBuddyChatUsesIsolatedPath(t *testing.T) {
	ts, cfg, _, calls, _, _ := newWorkBuddyHarness(t, false)
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	status, out, _ := do(t, "POST", ts.URL+"/p/workbuddy/v1/chat/completions", cfg.ClientToken, body)
	if status != 200 {
		t.Fatalf("status = %d body = %s", status, out)
	}
	if !strings.Contains(out, `"content":"ok"`) {
		t.Fatalf("unexpected body: %s", out)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls = %d", got)
	}
}

func TestWorkBuddyChannelRetry(t *testing.T) {
	ts, cfg, _, calls, bodies, _ := newWorkBuddyHarness(t, true)
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"You are opencode. Do work now."},{"role":"user","content":"hi"}],"stream":true}`
	status, out, _ := do(t, "POST", ts.URL+"/p/workbuddy/v1/chat/completions", cfg.ClientToken, body)
	if status != 200 {
		t.Fatalf("status = %d body = %s", status, out)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (channel retry)", got)
	}
	second := (*bodies)[1]
	if !strings.Contains(second, "You are a helpful assistant.\\n") && !strings.Contains(second, "You are a helpful assistant.\n") {
		t.Fatalf("second body missing neutral head: %s", second)
	}
	if !strings.Contains(second, "\u200b") && !strings.Contains(second, "\\u200b") {
		t.Fatalf("second body missing zero-width breaks: %s", second)
	}
}

func TestWorkBuddyRejectsNonStreamWithoutUpstream(t *testing.T) {
	ts, cfg, _, calls, _, _ := newWorkBuddyHarness(t, false)
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	status, _, _ := do(t, "POST", ts.URL+"/p/workbuddy/v1/chat/completions", cfg.ClientToken, body)
	if status != 400 {
		t.Fatalf("status = %d", status)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

func TestWorkBuddyNoAccount(t *testing.T) {
	ts, cfg, s, _, _, _ := newWorkBuddyHarness(t, false)
	if err := s.wb.Remove("user:u1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	status, out, _ := do(t, "POST", ts.URL+"/p/workbuddy/v1/chat/completions", cfg.ClientToken, body)
	if status != 503 || !strings.Contains(out, "workbuddy_no_account") {
		t.Fatalf("status = %d body = %s", status, out)
	}
}

func TestWorkBuddyAdminStatusHidesTokens(t *testing.T) {
	ts, cfg, _, _, _, _ := newWorkBuddyHarness(t, false)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/api/workbuddy/status", nil)
	req.Header.Set("X-Admin-Token", cfg.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "wb-access") || strings.Contains(text, "wb-refresh") {
		t.Fatalf("admin response leaked tokens: %s", text)
	}
	if !strings.Contains(text, "user:u1") {
		t.Fatalf("account missing: %s", text)
	}
}

func TestWorkBuddyRefreshesExpiredToken(t *testing.T) {
	ts, cfg, s, _, _, refreshes := newWorkBuddyHarness(t, false)
	acc, ok := s.wb.Find("user:u1")
	if !ok {
		t.Fatal("account missing")
	}
	acc.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
	if _, err := s.wb.Upsert(acc); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	status, out, _ := do(t, "POST", ts.URL+"/p/workbuddy/v1/chat/completions", cfg.ClientToken, body)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, out)
	}
	if got := atomic.LoadInt32(refreshes); got != 1 {
		t.Fatalf("refresh calls = %d", got)
	}
	updated, _ := s.wb.Find("user:u1")
	if updated.AccessToken != "wb-access-2" || updated.RefreshToken != "wb-refresh-2" {
		t.Fatalf("token not updated: %+v", updated)
	}
	if updated.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expiry not updated: %d", updated.ExpiresAt)
	}
}
