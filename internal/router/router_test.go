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

func TestExhaustedKeyRecoversWhenBalanceArrives(t *testing.T) {
	balances := map[string]float64{
		"ms-good-key-0001": 100, "ms-good-key-0002": 0,
		"sk_cline_key_0001": 50, "sk_cline_key_0002": 500,
	}
	r, _, closeFn := testRouter(t, balances)
	defer closeFn()
	ctx := context.Background()
	if _, err := r.Ensure(ctx, "modelscope"); err != nil {
		t.Fatal(err)
	}
	// 0002 判定魔粒不足（记录标记时间），0001 不受影响。
	_ = r.ReportFailure(ctx, "modelscope", "ms-good-key-0002", 429, "HTTP 429: insufficient balance")
	r.mu.Lock()
	if !r.ms.Exhausted[Hash("ms-good-key-0002")] {
		r.mu.Unlock()
		t.Fatal("0002 should be exhausted")
	}
	r.mu.Unlock()
	// 冷静期内即使余额到账也不解封。
	balances["ms-good-key-0002"] = 270
	r.RefreshProvider(ctx, "modelscope", true)
	r.mu.Lock()
	stillMarked := r.ms.Exhausted[Hash("ms-good-key-0002")]
	r.mu.Unlock()
	if !stillMarked {
		t.Fatal("must not recover inside grace period")
	}
	// 冷静期过后余额 > 0 自动解封，重新参与选 key。
	r.mu.Lock()
	r.ms.ExhaustedAt[Hash("ms-good-key-0002")] = time.Now().Add(-2 * msExhaustedGrace)
	r.mu.Unlock()
	r.RefreshProvider(ctx, "modelscope", true)
	r.mu.Lock()
	recovered := !r.ms.Exhausted[Hash("ms-good-key-0002")]
	r.mu.Unlock()
	if !recovered {
		t.Fatal("0002 should recover after grace period when balance > 0")
	}
	k, err := r.Ensure(ctx, "modelscope")
	if err != nil {
		t.Fatal(err)
	}
	if k != "ms-good-key-0001" {
		t.Fatalf("current healthy key should stay sticky, got %q", k)
	}
	// 解封后的 key 必须重新进入候选池：当前 key 失败时能切过去。
	_ = r.ReportFailure(ctx, "modelscope", "ms-good-key-0001", 429, "HTTP 429: insufficient balance")
	next, err := r.Ensure(ctx, "modelscope")
	if err != nil || next != "ms-good-key-0002" {
		t.Fatalf("recovered key should be selectable, got %q err=%v", next, err)
	}
}

func TestAMDAndDeepSeekBalances(t *testing.T) {
	home := t.TempDir()
	p := config.DefaultPaths(home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/amd/v1/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"daily_cost_limit_usd": 1.0, "daily_cost_remaining_usd": 0.75,
			})
		case "/ds/user/balance":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"is_available": true,
				"balance_infos": []any{
					map[string]any{"currency": "USD", "total_balance": "99.00"},
					map[string]any{"currency": "CNY", "total_balance": "6.24"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Home: home, Bind: "127.0.0.1", Port: 8788,
		ClientToken: "client-token-1234567890", AdminToken: "admin-token-1234567890",
		Providers: map[string]*config.Provider{
			"amd":      {ID: "amd", Label: "amd", BaseURL: srv.URL + "/amd/v1", Models: []string{"m"}},
			"deepseek": {ID: "deepseek", Label: "ds", BaseURL: srv.URL + "/ds/v1", Models: []string{"d"}},
			"atria":    {ID: "atria", Label: "at", BaseURL: srv.URL + "/atria/v1", Models: []string{"a"}},
		},
		Limits: config.Limits{MaxBodyBytes: 1024 * 1024, MaxConcurrent: 8, HeadersTimeoutMS: 5000, TotalTimeoutMS: 10000, MaxRetries: 2},
		Paths:  p,
	}
	cfg.Normalize()
	if err := os.MkdirAll(filepath.Dir(p.Keys), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := `{"amd":{"key":"rc-1"},"deepseek":{"key":"sk-ds"},"atria":{"keys":["atr-1"]}}`
	if err := os.WriteFile(p.Keys, []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := logx.New(p.Logs, 1024*1024, 2)
	r := New(cfg, log)
	ctx := context.Background()
	r.RefreshProvider(ctx, "amd", true)
	r.RefreshProvider(ctx, "deepseek", true)
	r.RefreshProvider(ctx, "atria", true)
	status := map[string]ProviderStatus{}
	for _, ps := range r.Status() {
		status[ps.ID] = ps
	}
	amd := status["amd"]
	if !amd.BalanceSupported || len(amd.Keys) != 1 || amd.Keys[0].Balance == nil || *amd.Keys[0].Balance != 0.75 {
		t.Fatalf("amd balance not fetched: %+v", amd)
	}
	ds := status["deepseek"]
	if !ds.BalanceSupported || len(ds.Keys) != 1 || ds.Keys[0].Balance == nil || *ds.Keys[0].Balance != 6.24 {
		t.Fatalf("deepseek balance not fetched: %+v", ds)
	}
	at := status["atria"]
	if at.BalanceSupported {
		t.Fatalf("atria should not claim balance support: %+v", at)
	}
}
