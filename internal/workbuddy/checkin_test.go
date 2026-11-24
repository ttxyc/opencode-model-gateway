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

type checkinFake struct {
	statusBody string
	claimBody  string
	status     int32
	claim      int32
}

func newCheckinManager(t *testing.T, accounts ...Account) (*Manager, *checkinFake) {
	t.Helper()
	fake := &checkinFake{
		statusBody: `{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":false,"streak_days":5,"daily_credit":100,"activity_name":"每日签到"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":100,"streak_days":6,"is_streak_day":false}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Client-Platform"); got != "web" {
			t.Errorf("x-client-platform = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Errorf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) != "{}" {
			t.Errorf("body = %q", body)
		}
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			atomic.AddInt32(&fake.status, 1)
			_, _ = io.WriteString(w, fake.statusBody)
		case "/v2/billing/meter/daily-checkin":
			atomic.AddInt32(&fake.claim, 1)
			_, _ = io.WriteString(w, fake.claimBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	m := NewManager(filepath.Join(t.TempDir(), "workbuddy.json"), nil)
	ep := DefaultEndpoints()
	ep.CNBillingBase = srv.URL
	ep.GlobalBillingBase = srv.URL
	m.SetEndpoints(ep)
	for _, acc := range accounts {
		if _, err := m.Upsert(acc); err != nil {
			t.Fatal(err)
		}
	}
	return m, fake
}

func testAccount(id, label, region string) Account {
	return Account{
		ID: id, Label: label, Region: region, UserID: strings.TrimPrefix(id, "user:"),
		AccessToken: "access", RefreshToken: "refresh",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
}

func TestRunCheckinDryRunThenClaim(t *testing.T) {
	m, fake := newCheckinManager(t, testAccount("user:u1", "tester", RegionCN))

	dry, err := m.RunCheckin(context.Background(), RegionCN, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Eligible != 1 || dry.Success != 0 || dry.Failed != 0 {
		t.Fatalf("dry result = %+v", dry)
	}
	if len(dry.Results) != 1 || dry.Results[0].State != "eligible" {
		t.Fatalf("dry item = %+v", dry.Results)
	}
	if got := atomic.LoadInt32(&fake.claim); got != 0 {
		t.Fatalf("dry run called claim %d times", got)
	}

	got, err := m.RunCheckin(context.Background(), RegionCN, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Success != 1 || got.TotalCredit != 100 || got.Failed != 0 {
		t.Fatalf("claim result = %+v", got)
	}
	item := got.Results[0]
	if item.State != "claimed" || !item.Claimed || item.Credit != 100 || item.StreakDays != 6 {
		t.Fatalf("claim item = %+v", item)
	}
	if calls := atomic.LoadInt32(&fake.status); calls != 2 {
		t.Fatalf("status calls = %d, want 2", calls)
	}
	if calls := atomic.LoadInt32(&fake.claim); calls != 1 {
		t.Fatalf("claim calls = %d, want 1", calls)
	}
}

func TestRunCheckinAlreadyInactiveAndTopLevelClaim(t *testing.T) {
	m, fake := newCheckinManager(t, testAccount("user:u1", "tester", RegionCN))
	fake.statusBody = `{"code":0,"data":{"active":true,"today_checked_in":true,"streak_days":9,"daily_credit":100}}`

	res, err := m.RunCheckin(context.Background(), RegionCN, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Already != 1 || res.Success != 0 || res.Failed != 0 {
		t.Fatalf("already result = %+v", res)
	}
	if got := atomic.LoadInt32(&fake.claim); got != 0 {
		t.Fatalf("already called claim %d times", got)
	}

	fake.statusBody = `{"code":0,"data":{"active":false}}`
	res, err = m.RunCheckin(context.Background(), RegionCN, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("inactive result = %+v", res)
	}
	if res.Results[0].State != "skipped" || res.Results[0].Message != "签到活动未开启" {
		t.Fatalf("inactive item = %+v", res.Results[0])
	}

	fake.statusBody = `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":9,"daily_credit":100}}`
	fake.claimBody = `{"code":0,"msg":"OK","credit":100,"streak_days":10,"is_streak_day":false}`
	res, err = m.RunCheckin(context.Background(), RegionCN, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success != 1 || res.TotalCredit != 100 {
		t.Fatalf("top-level result = %+v", res)
	}
	if item := res.Results[0]; item.StreakDays != 10 || item.Credit != 100 {
		t.Fatalf("top-level item = %+v", item)
	}
}

func TestRunCheckinNotEligible(t *testing.T) {
	m, fake := newCheckinManager(t, testAccount("user:u1", "tester", RegionCN))
	fake.claimBody = `{"code":1002,"msg":"activity not eligible"}`

	res, err := m.RunCheckin(context.Background(), RegionCN, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Failed != 0 || res.OK != true {
		t.Fatalf("result = %+v", res)
	}
	if item := res.Results[0]; item.State != "skipped" || !strings.Contains(item.Message, "activity not eligible") {
		t.Fatalf("item = %+v", item)
	}
}

func TestRunCheckinAllRegions(t *testing.T) {
	m, _ := newCheckinManager(t,
		testAccount("user:u1", "cn", RegionCN),
		testAccount("user:u2", "global", RegionGlobal),
	)

	res, err := m.RunCheckin(context.Background(), "all", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v", res.Results)
	}
	if res.Results[0].Region == res.Results[1].Region {
		t.Fatalf("expected one cn and one global: %+v", res.Results)
	}
}
