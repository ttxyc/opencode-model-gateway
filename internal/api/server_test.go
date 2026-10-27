package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/router"
)

type upstreamState struct {
	mu   sync.Mutex
	keys []string
	bad  map[string]bool
}

func (u *upstreamState) add(k string) {
	u.mu.Lock()
	u.keys = append(u.keys, k)
	u.mu.Unlock()
}

func newTestServer(t *testing.T) (*httptest.Server, *upstreamState, *config.Config, *router.Router) {
	t.Helper()
	home := t.TempDir()
	paths := config.DefaultPaths(home)
	up := &upstreamState{bad: map[string]bool{}}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/openapi/v1/magicubes/balance":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"available_balance": 100}})
			return
		case "/api/v1/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "user"}})
			return
		case "/api/v1/users/user/balance":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"balance": 200}})
			return
		case "/v1/chat/completions", "/api/v1/chat/completions", "/amd/v1/chat/completions", "/ds/v1/chat/completions":
			up.add(key)
			up.mu.Lock()
			bad := up.bad[key]
			up.mu.Unlock()
			if bad {
				w.WriteHeader(402)
				_, _ = w.Write([]byte(`{"error":{"message":"insufficient balance"}}`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "wrap-test" {
				w.Header().Set("content-type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"choices":[{"message":{"content":"wrapped"}}],"usage":{"total_tokens":2}}}`)
				return
			}
			if body["stream"] == true {
				w.Header().Set("content-type", "text/event-stream")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				return
			}
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":2}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(mock.Close)
	t.Setenv("KEY_ROUTER_MS_BALANCE_URL", mock.URL+"/openapi/v1/magicubes/balance")
	t.Setenv("KEY_ROUTER_CLINE_API_BASE", mock.URL+"/api/v1")
	cfg := &config.Config{
		Home: home, Bind: "127.0.0.1", Port: 8788,
		ClientToken: "client-token-1234567890", AdminToken: "admin-token-1234567890",
		Providers: map[string]*config.Provider{
			"modelscope": {ID: "modelscope", Label: "ms", BaseURL: mock.URL + "/v1", Models: []string{"deepseek-ai/DeepSeek-V4.1-Flash-1017"}},
			"cline":      {ID: "cline", Label: "cl", BaseURL: mock.URL + "/api/v1", Models: []string{"cohere/north-mini-code:free"}},
			"amd":        {ID: "amd", Label: "amd", BaseURL: mock.URL + "/amd/v1", Models: []string{"Qwen3.8-Flash-Next"}},
			"deepseek":   {ID: "deepseek", Label: "ds", BaseURL: mock.URL + "/ds/v1", Models: []string{"deepseek-flash"}},
		},
		Limits: config.Limits{MaxBodyBytes: 4 * 1024 * 1024, MaxConcurrent: 8, HeadersTimeoutMS: 5000, TotalTimeoutMS: 10000, MaxRetries: 2},
		Paths:  paths,
	}
	cfg.Normalize()
	if err := os.MkdirAll(filepath.Dir(paths.Keys), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := `{"modelscope":{"keys":["ms-good-key-0001","ms-good-key-0002"]},"cline":{"keys":["sk_cline_key_0001"]},"amd":{"key":"rc-00000000000001"},"deepseek":{"key":"sk-ds-key-0001"}}`
	if err := os.WriteFile(paths.Keys, []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := logx.New(paths.Logs, 1024*1024, 2)
	r := router.New(cfg, log)
	r.RefreshProvider(context.Background(), "modelscope", true)
	r.RefreshProvider(context.Background(), "cline", true)
	s := New(cfg, r, log, Options{Version: "test", StartedAt: time.Now(), AdminToken: cfg.AdminToken, ClientToken: cfg.ClientToken})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, up, cfg, r
}

func do(t *testing.T, method, url, token string, body string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestModelsAndChat(t *testing.T) {
	ts, up, cfg, _ := newTestServer(t)
	if code, body, _ := do(t, "GET", ts.URL+"/healthz", "", ""); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("health %d %s", code, body)
	}
	if code, _, _ := do(t, "GET", ts.URL+"/v1/models", "", ""); code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
	code, body, _ := do(t, "GET", ts.URL+"/v1/models", cfg.ClientToken, "")
	if code != 200 || !strings.Contains(body, "modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017") {
		t.Fatalf("models %d %s", code, body)
	}
	code, body, _ = do(t, "POST", ts.URL+"/v1/chat/completions", cfg.ClientToken, `{"model":"modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("chat %d %s", code, body)
	}
	up.mu.Lock()
	if len(up.keys) == 0 || up.keys[0] != "ms-good-key-0001" {
		t.Fatalf("upstream keys %v", up.keys)
	}
	up.mu.Unlock()
}

func TestFailoverAndStream(t *testing.T) {
	ts, up, cfg, _ := newTestServer(t)
	up.bad["ms-good-key-0001"] = true
	code, body, _ := do(t, "POST", ts.URL+"/v1/chat/completions", cfg.ClientToken, `{"model":"modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017","messages":[]}`)
	if code != 200 || !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("failover %d %s", code, body)
	}
	code, body, hdr := do(t, "POST", ts.URL+"/v1/chat/completions", cfg.ClientToken, `{"model":"modelscope/deepseek-ai/DeepSeek-V4.1-Flash-1017","stream":true,"messages":[]}`)
	if code != 200 || !strings.Contains(hdr.Get("Content-Type"), "text/event-stream") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream %d %s %v", code, body, hdr)
	}
}

func TestClineUnwrap(t *testing.T) {
	ts, _, cfg, _ := newTestServer(t)
	code, body, _ := do(t, "POST", ts.URL+"/p/cline/v1/chat/completions", cfg.ClientToken, `{"model":"wrap-test","messages":[]}`)
	if code != 200 || !strings.Contains(body, `"choices"`) || !strings.Contains(body, "wrapped") {
		t.Fatalf("cline %d %s", code, body)
	}
	if strings.Contains(body, `"data"`) {
		t.Fatalf("unexpected data wrapper: %s", body)
	}
}

func TestAdmin(t *testing.T) {
	ts, _, cfg, _ := newTestServer(t)
	code, body, _ := do(t, "GET", ts.URL+"/admin/api/status", cfg.AdminToken, "")
	if code != 200 || !strings.Contains(body, "providers") {
		t.Fatalf("admin status %d %s", code, body)
	}
	if code, _, _ := do(t, "GET", ts.URL+"/admin/api/status", cfg.ClientToken, ""); code != 401 {
		t.Fatalf("expected admin 401, got %d", code)
	}
	code, body, _ = do(t, "POST", ts.URL+"/admin/api/keys", cfg.AdminToken, `{"provider":"modelscope","key":"ms-good-key-0003"}`)
	if code != 200 || !strings.Contains(body, "ms-good...0003") {
		t.Fatalf("admin add %d %s", code, body)
	}
}

func TestImportKeysMergeAndOverwrite(t *testing.T) {
	ts, _, cfg, r := newTestServer(t)
	body := `{"content":"{\"modelscope\":{\"keys\":[\"ms-good-key-0001\",\"ms-new-key-1\"]}}","overwrite":false}`
	code, out, _ := do(t, "POST", ts.URL+"/admin/api/import-keys", cfg.AdminToken, body)
	if code != 200 || !strings.Contains(out, `"added":1`) || !strings.Contains(out, `"已存在"`) {
		t.Fatalf("import merge %d %s", code, out)
	}
	keys := r.Keys("modelscope")
	if len(keys) != 3 || keys[2] != "ms-new-key-1" {
		t.Fatalf("merge keys %v", keys)
	}
	body = `{"content":"{\"modelscope\":{\"keys\":[\"ms-overwrite-key\"]}}","overwrite":true}`
	code, out, _ = do(t, "POST", ts.URL+"/admin/api/import-keys", cfg.AdminToken, body)
	if code != 200 || !strings.Contains(out, `"added":1`) {
		t.Fatalf("import overwrite %d %s", code, out)
	}
	keys = r.Keys("modelscope")
	if len(keys) != 1 || keys[0] != "ms-overwrite-key" {
		t.Fatalf("overwrite keys %v", keys)
	}
}

func TestImportKeysRejectsUnknownAndBadJSON(t *testing.T) {
	ts, _, cfg, _ := newTestServer(t)
	code, out, _ := do(t, "POST", ts.URL+"/admin/api/import-keys", cfg.AdminToken, `{"content":"not json"}`)
	if code != 400 {
		t.Fatalf("bad json got %d %s", code, out)
	}
	code, out, _ = do(t, "POST", ts.URL+"/admin/api/import-keys", cfg.AdminToken, `{"content":"{\"unknown\":{\"keys\":[\"x\"]}}"}`)
	if code != 200 || !strings.Contains(out, "未知服务商") {
		t.Fatalf("unknown provider %d %s", code, out)
	}
	if code, _, _ = do(t, "POST", ts.URL+"/admin/api/import-keys", cfg.ClientToken, `{"content":"{}"}`); code != 401 {
		t.Fatalf("expected admin auth, got %d", code)
	}
}

func TestExportKeys(t *testing.T) {
	ts, _, cfg, _ := newTestServer(t)
	code, body, hdr := do(t, "GET", ts.URL+"/admin/api/export-keys", cfg.AdminToken, "")
	if code != 200 || !strings.Contains(body, `"modelscope"`) || !strings.Contains(body, `"keys"`) {
		t.Fatalf("export %d %s", code, body)
	}
	if !strings.Contains(hdr.Get("Content-Disposition"), "quota-line.json") {
		t.Fatalf("content-disposition %q", hdr.Get("Content-Disposition"))
	}
	if code, _, _ := do(t, "GET", ts.URL+"/admin/api/export-keys", cfg.ClientToken, ""); code != 401 {
		t.Fatalf("expected admin auth, got %d", code)
	}
}
