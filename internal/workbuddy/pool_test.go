package workbuddy

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(filepath.Join(t.TempDir(), "workbuddy.json"), nil)
}

func addTestAccount(t *testing.T, m *Manager, id, region string) {
	t.Helper()
	_, err := m.Upsert(Account{
		ID: id, Label: id, Region: region, Domain: defaultDomain(region),
		UserID: id, AccessToken: "access-" + id, RefreshToken: "refresh-" + id,
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPoolRoundRobinAndSkip(t *testing.T) {
	m := newTestManager(t)
	addTestAccount(t, m, "user:a", RegionCN)
	addTestAccount(t, m, "user:b", RegionCN)
	addTestAccount(t, m, "user:c", RegionCN)

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		a := m.Pick(RegionCN, nil)
		if a == nil {
			t.Fatal("nil pick")
		}
		seen[a.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("round robin did not cover all accounts: %v", seen)
	}

	m.Report("user:a", "invalid", "bad", 0)
	for i := 0; i < 6; i++ {
		a := m.Pick(RegionCN, nil)
		if a != nil && a.ID == "user:a" {
			t.Fatal("invalid account was picked")
		}
	}
	m.Reset("user:a")
	if a := m.Pick(RegionCN, nil); a == nil {
		t.Fatal("reset account not eligible")
	}
}

func TestPoolExhaustedCooldown(t *testing.T) {
	m := newTestManager(t)
	addTestAccount(t, m, "user:a", RegionCN)
	m.Report("user:a", "exhausted", "credits", int64(time.Hour/time.Millisecond))
	if a := m.Pick(RegionCN, nil); a != nil {
		t.Fatal("cooling account picked")
	}
	// A past cooldown becomes eligible again.
	m.Report("user:a", "exhausted", "credits", 1)
	time.Sleep(5 * time.Millisecond)
	if a := m.Pick(RegionCN, nil); a == nil {
		t.Fatal("expired cooldown not eligible")
	}
}

func TestPoolRegionIsolation(t *testing.T) {
	m := newTestManager(t)
	addTestAccount(t, m, "user:a", RegionCN)
	addTestAccount(t, m, "user:g", RegionGlobal)
	if a := m.Pick(RegionCN, nil); a == nil || a.ID != "user:a" {
		t.Fatalf("cn pick = %+v", a)
	}
	if a := m.Pick(RegionGlobal, nil); a == nil || a.ID != "user:g" {
		t.Fatalf("global pick = %+v", a)
	}
}

func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy.json")
	m := NewManager(path, nil)
	addTestAccount(t, m, "user:a", RegionCN)
	m2 := NewManager(path, nil)
	if len(m2.Snapshot()) != 1 {
		t.Fatalf("persisted accounts = %d", len(m2.Snapshot()))
	}
}
