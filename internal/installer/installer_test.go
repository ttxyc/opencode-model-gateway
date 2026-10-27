package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepare(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "gateway.exe")
	if err := os.WriteFile(exe, []byte("PE"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := Prepare(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer Cleanup(dir)
	for _, rel := range []string{
		"gateway.exe", "config.example.json", "publickey.txt", "opencode-gateway.vbs",
		"scripts/install.ps1", "scripts/install-wsl.ps1",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
}
