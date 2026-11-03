package workbuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dshYAML(t *testing.T, session string) string {
	t.Helper()
	// Single-quoted YAML scalar, as written by DSH.
	return "version: 4\nrefs:\n  WORKBUDDY_LOGIN_SESSIONS: '" + strings.ReplaceAll(session, "'", "''") + "'\n  OTHER: 'secret'\n"
}

func TestExtractYAMLScalar(t *testing.T) {
	raw := dshYAML(t, `{"version":1,"sessions":[]}`)
	got, ok := extractYAMLScalar(raw, "WORKBUDDY_LOGIN_SESSIONS")
	if !ok || got != `{"version":1,"sessions":[]}` {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := extractYAMLScalar(raw, "MISSING"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestExtractYAMLScalarFoldsLines(t *testing.T) {
	raw := "refs:\n  WORKBUDDY_LOGIN_SESSIONS: '{\"a\":\"x\n  y\n  z\"}'\n  OTHER: 'secret'\n"
	got, ok := extractYAMLScalar(raw, "WORKBUDDY_LOGIN_SESSIONS")
	if !ok {
		t.Fatal("not found")
	}
	want := `{"a":"x y z"}`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestImportDSH(t *testing.T) {
	now := time.Now().UnixMilli()
	store := map[string]any{
		"version":  1,
		"activeId": "user:u1",
		"sessions": []any{
			map[string]any{
				"id": "user:u1", "label": "tester",
				"auth": map[string]any{
					"accessToken": "access-1", "refreshToken": "refresh-1",
					"domain": "www.codebuddy.cn", "expiresAt": now + 86400000,
					"expiresIn": 2592000, "refreshExpiresIn": 5183999,
				},
				"account": map[string]any{"userId": "u1", "displayName": "tester"},
			},
			map[string]any{
				"id": "user:u2", "label": "tester2",
				"auth": map[string]any{
					"accessToken": "access-2", "refreshToken": "refresh-2",
					"domain": "workbuddy.ai", "expiresAt": now + 86400000,
				},
				"account": map[string]any{"userId": "u2", "displayName": "tester2"},
			},
		},
	}
	raw, _ := json.Marshal(store)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.yaml")
	if err := os.WriteFile(cred, []byte(dshYAML(t, string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(dir, "workbuddy.json"), nil)
	res, err := m.ImportDSH(cred)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 2 {
		t.Fatalf("imported = %d, errors=%v", res.Imported, res.Errors)
	}
	views := m.Snapshot()
	if len(views) != 2 {
		t.Fatalf("accounts = %d", len(views))
	}
	byID := map[string]AccountView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if byID["user:u1"].Region != RegionCN || byID["user:u2"].Region != RegionGlobal {
		t.Fatalf("regions wrong: %+v", byID)
	}

	// Re-import must update in place, not duplicate.
	res, err = m.ImportDSH(cred)
	if err != nil || res.Imported != 2 {
		t.Fatalf("reimport: %+v err=%v", res, err)
	}
	if len(m.Snapshot()) != 2 {
		t.Fatalf("duplicated accounts: %d", len(m.Snapshot()))
	}
}
