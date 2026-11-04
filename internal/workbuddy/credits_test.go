package workbuddy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePersonalCredits(t *testing.T) {
	accounts := []map[string]any{
		{
			"CycleCapacityRemainPrecise": 1000.5,
			"CycleCapacitySizePrecise":   2000,
			"PackageName":                "A",
		},
		{
			"SlicePeriodUsageDetails": []any{
				map[string]any{
					"SlicePeriodCapacityRemainPrecise": 100,
					"SlicePeriodEndTime":               "2026-12-01 00:00:00",
					"PackageName":                      "B",
				},
				map[string]any{
					"SlicePeriodCapacityRemainPrecise": 50,
					"SlicePeriodEndTime":               "2026-11-15 00:00:00",
					"PackageName":                      "B",
				},
			},
		},
	}
	got, err := parsePersonalCredits(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Credits == nil || *got.Credits != 1150.5 {
		t.Fatalf("credits = %v", got.Credits)
	}
	if len(got.Segments) != 3 {
		t.Fatalf("segments = %d: %+v", len(got.Segments), got.Segments)
	}
	if got.Segments[0].ExpiresAt == 0 {
		t.Fatalf("segments not sorted: %+v", got.Segments)
	}
}

func TestParseEnterpriseCredits(t *testing.T) {
	got, err := parseEnterpriseCredits(map[string]any{
		"data": map[string]any{"limitNum": float64(1000), "credit": float64(250), "cycleResetTime": float64(1796295707)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Credits == nil || *got.Credits != 750 {
		t.Fatalf("credits = %v", got.Credits)
	}
	if got.ResetAt != 1796295707000 {
		t.Fatalf("reset = %d", got.ResetAt)
	}
}

func TestBillingHost(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "workbuddy.json"), nil)
	cases := []struct {
		acc  Account
		want string
	}{
		{Account{Region: RegionCN, Domain: "www.codebuddy.cn"}, "https://www.codebuddy.cn"},
		{Account{Region: RegionCN, Domain: "www.workbuddy.cn"}, "https://www.workbuddy.cn"},
		{Account{Region: RegionGlobal, Domain: "www.workbuddy.ai"}, "https://www.workbuddy.ai"},
		{Account{Region: RegionGlobal, Domain: "www.codebuddy.ai"}, "https://www.codebuddy.ai"},
		{Account{Region: RegionCN}, "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		if got := m.billingHost(c.acc); got != c.want {
			t.Fatalf("billingHost(%+v) = %s, want %s", c.acc, got, c.want)
		}
	}
}

func TestFetchCreditsAndCache(t *testing.T) {
	var calls int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-user-resource" {
			w.WriteHeader(404)
			return
		}
		atomic.AddInt32(&calls, 1)
		if got := r.Header.Get("X-Client-Platform"); got != "web" {
			t.Errorf("x-client-platform = %q", got)
		}
		if got := r.Header.Get("X-User-Id"); got != "u1" {
			t.Errorf("x-user-id = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"ProductCode":"p_tcaca"`) {
			t.Errorf("unexpected body: %s", body)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"Response":{"Data":{"Accounts":[{"CycleCapacityRemainPrecise":1234.5}]}}}}`)
	}))
	defer fake.Close()

	m := NewManager(filepath.Join(t.TempDir(), "workbuddy.json"), nil)
	ep := DefaultEndpoints()
	ep.CNBillingBase = fake.URL
	m.SetEndpoints(ep)
	if _, err := m.Upsert(Account{
		ID: "user:u1", Label: "tester", Region: RegionCN, UserID: "u1",
		AccessToken: "access", RefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := m.FetchCredits(context.Background(), RegionCN)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total == nil || *res.Total != 1234.5 {
		t.Fatalf("total = %v", res.Total)
	}
	if _, err := m.FetchCredits(context.Background(), RegionCN); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("billing calls = %d, want 1 (cache)", got)
	}
	// A store change invalidates the cache.
	if err := m.Reset("user:u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FetchCredits(context.Background(), RegionCN); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("billing calls after reset = %d, want 2", got)
	}
}
