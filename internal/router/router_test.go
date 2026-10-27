package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

func testRouter(t *testing.T, balances map[string]float64) (*Router, *config.Config, func()) {
	t.Helper()
	home := t.TempDir()
	p := config.DefaultPaths(home)
	cfg := &config.Config{
		Home: home, Bind: "127.0.0.1", Port: 8788,
		ClientToken: "client-token-1234567890", AdminToken: "admin-token-1234567890",
		Providers: map[string]*config.Provider{
			"modelscope": {ID: "modelscope", Label: "ms", BaseURL: "https://example.invalid/v1", Models: []string{"m"}},
			"cline":      {ID: "cline", Label: "cl", BaseURL: "https://example.invalid/api/v1", Models: []string{"c"}},
			"atria":      {ID: "atria", Label: "at", BaseURL: "https://example.invalid/atria/v1", Models: []string{"a"}},
			"amd":        {ID: "amd", Label: "amd", BaseURL: "https://example.invalid/amd/v1", Models: []string{"am"}},
		},
		Limits: config.Limits{MaxBodyBytes: 1024 * 1024, MaxConcurrent: 8, HeadersTimeoutMS: 5000, TotalTimeoutMS: 10000, MaxRetries: 2},
		Paths:  p,
	}
	cfg.Normalize()
	if err := os.MkdirAll(filepath.Dir(p.Keys), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := map[string]any{
		"modelscope": map[string]any{"keys": []string{"ms-good-key-0001", "ms-good-key-0002"}},
		"cline":      map[string]any{"keys": []string{"sk_cline_key_0001", "sk_cline_key_0002"}},
		"atria":      map[string]any{"keys": []string{"atr_key_0001", "atr_key_0002"}},
		"amd":        map[string]any{"key": "rc-00000000000001"},
	}
	b, _ := json.Marshal(keys)
	if err := os.WriteFile(p.Keys, b, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := logx.New(p.Logs, 1024*1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if len(key) > 7 {
			key = key[7:]
		}
		switch r.URL.Path {
		case "/openapi/v1/magicubes/balance", "/v1/openapi/v1/magicubes/balance":
			v, ok := balances[key]
			if !ok {
				http.Error(w, "bad key", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"available_balance": v}})
		case "/api/v1/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "u-" + key}})
		case "/api/v1/users/" + "u-" + key + "/balance":
			v, ok := balances[key]
			if !ok {
				http.Error(w, "bad key", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"balance": v}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("KEY_ROUTER_MS_BALANCE_URL", srv.URL+"/openapi/v1/magicubes/balance")
	t.Setenv("KEY_ROUTER_CLINE_API_BASE", srv.URL+"/api/v1")
	r := New(cfg, log)
	return r, cfg, srv.Close
}

func TestModelScopeStickyAndFailover(t *testing.T) {
	r, _, closeFn := testRouter(t, map[string]float64{
		"ms-good-key-0001": 100, "ms-good-key-0002": 10,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	})
	defer closeFn()
	ctx := context.Background()
	k, err := r.Ensure(ctx, "modelscope")
	if err != nil || k != "ms-good-key-0001" {
		t.Fatalf("got %q err=%v", k, err)
	}
	if k2, _ := r.Ensure(ctx, "modelscope"); k2 != k {
		t.Fatalf("not sticky: %q vs %q", k2, k)
	}
	if err := r.ReportFailure(ctx, "modelscope", k, 402, "HTTP 402: insufficient balance"); err != nil {
		t.Fatal(err)
	}
	k2, err := r.Ensure(ctx, "modelscope")
	if err != nil || k2 != "ms-good-key-0002" {
		t.Fatalf("failover got %q err=%v", k2, err)
	}
}

func TestClineAndAtria(t *testing.T) {
	r, _, closeFn := testRouter(t, map[string]float64{
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
		"ms-good-key-0001": 100, "ms-good-key-0002": 10,
	})
	defer closeFn()
	ctx := context.Background()
	k, err := r.Ensure(ctx, "cline")
	if err != nil || k != "sk_cline_key_0002" {
		t.Fatalf("cline got %q err=%v", k, err)
	}
	_ = r.ReportFailure(ctx, "cline", k, 429, "rate limit reached, try again in 10m")
	k2, err := r.Ensure(ctx, "cline")
	if err != nil || k2 != "sk_cline_key_0001" {
		t.Fatalf("cline failover got %q err=%v", k2, err)
	}
	a, _ := r.Ensure(ctx, "atria")
	if a != "atr_key_0001" {
		t.Fatalf("atria first got %q", a)
	}
	_ = r.ReportFailure(ctx, "atria", a, 429, "quota exhausted")
	a2, _ := r.Ensure(ctx, "atria")
	if a2 != "atr_key_0002" {
		t.Fatalf("atria failover got %q", a2)
	}
}

func TestAllExhaustedRecovery(t *testing.T) {
	balances := map[string]float64{
		"ms-good-key-0001": 0, "ms-good-key-0002": 0,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	}
	r, _, closeFn := testRouter(t, balances)
	defer closeFn()
	ctx := context.Background()
	r.RefreshProvider(ctx, "modelscope", true)
	if _, err := r.Ensure(ctx, "modelscope"); err == nil {
		t.Fatal("expected all exhausted")
	}
	balances["ms-good-key-0001"] = 42
	r.mu.Lock()
	r.ms.LastProbe = time.Time{}
	r.mu.Unlock()
	k, err := r.Ensure(ctx, "modelscope")
	if err != nil || k != "ms-good-key-0001" {
		t.Fatalf("recovery got %q err=%v", k, err)
	}
}

func TestAddDeleteAndAllExhausted(t *testing.T) {
	r, cfg, closeFn := testRouter(t, map[string]float64{
		"ms-good-key-0001": 0, "ms-good-key-0002": 0,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	})
	defer closeFn()
	ctx := context.Background()
	r.RefreshProvider(ctx, "modelscope", true)
	if _, err := r.Ensure(ctx, "modelscope"); err == nil {
		t.Fatal("expected exhausted")
	}
	if err := r.AddKey("modelscope", "ms-good-key-0003"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg.Paths.Keys)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if _, ok := doc["modelscope"]; !ok {
		t.Fatal("key not persisted")
	}
	hash := Hash("ms-good-key-0003")
	if err := r.DeleteKey("modelscope", hash); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if len(r.Keys("modelscope")) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(r.Keys("modelscope")))
	}
}

func TestLoadPersistedBalancesAndCooldowns(t *testing.T) {
	balances := map[string]float64{
		"ms-good-key-0001": 1, "ms-good-key-0002": 2,
		"sk_cline_key_0001": 100, "sk_cline_key_0002": 200,
	}
	r, cfg, closeFn := testRouter(t, balances)
	defer closeFn()
	now := time.Now().UTC()
	state := map[string]any{
		"providers": []any{
			map[string]any{
				"id": "modelscope",
				"keys": []any{
					map[string]any{"keyHash": Hash("ms-good-key-0001"), "balance": 777.0, "balanceAt": now},
				},
			},
			map[string]any{
				"id": "cline",
				"keys": []any{
					map[string]any{"keyHash": Hash("sk_cline_key_0001"), "balance": 900.0, "balanceAt": now, "cooldownUntil": now.Add(time.Hour)},
					map[string]any{"keyHash": Hash("sk_cline_key_0002"), "balance": 100.0, "balanceAt": now},
				},
			},
		},
	}
	b, _ := json.Marshal(state)
	if err := os.MkdirAll(filepath.Dir(cfg.Paths.State), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Paths.State, b, 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := logx.New(cfg.Paths.Logs, 1024*1024, 2)
	r2 := New(cfg, log)
	r2.mu.Lock()
	got := r2.stateLocked("modelscope", "ms-good-key-0001")
	if got.Balance == nil || *got.Balance != 777 || !got.BalanceFromDisk {
		t.Fatalf("persisted ms balance not loaded: %+v", got)
	}
	r2.mu.Unlock()
	// cooldown must survive restart: key1 has 900 balance but is cooled, key2 wins.
	k, err := r2.Ensure(context.Background(), "cline")
	if err != nil || k != "sk_cline_key_0002" {
		t.Fatalf("cooldown not restored, got %q err=%v", k, err)
	}
	_ = r
}

func TestAllExhaustedThrottleDoesNotRefresh(t *testing.T) {
	r, _, closeFn := testRouter(t, map[string]float64{
		"ms-good-key-0001": 0, "ms-good-key-0002": 0,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	})
	defer closeFn()
	ctx := context.Background()
	if _, err := r.Ensure(ctx, "modelscope"); err == nil {
		t.Fatal("expected exhausted")
	}
	r.mu.Lock()
	probe := r.ms.LastProbe
	r.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if _, err := r.Ensure(ctx, "modelscope"); err == nil {
		t.Fatal("expected throttled exhausted")
	}
	r.mu.Lock()
	after := r.ms.LastProbe
	r.mu.Unlock()
	if !after.Equal(probe) {
		t.Fatalf("probe timestamp changed inside 5s throttle: %v -> %v", probe, after)
	}
}

func TestRefreshRetriesAfterAllBalancesUnknown(t *testing.T) {
	r, _, closeFn := testRouter(t, map[string]float64{
		"ms-good-key-0001": 100, "ms-good-key-0002": 10,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	})
	defer closeFn()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n <= 2 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"available_balance": 123}})
	}))
	defer srv.Close()
	t.Setenv("KEY_ROUTER_MS_BALANCE_URL", srv.URL)
	start := time.Now()
	r.refreshBalancesWithRetry(context.Background(), "modelscope", nil)
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("expected 800ms retry, elapsed=%v", elapsed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range []string{"ms-good-key-0001", "ms-good-key-0002"} {
		s := r.stateLocked("modelscope", k)
		if s.Balance == nil || *s.Balance != 123 {
			t.Fatalf("%s balance not refreshed after retry: %+v", k, s)
		}
	}
}
