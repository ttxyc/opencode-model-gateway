package installer

import (
	"bytes"
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
		"scripts/workbuddy-checkin.ps1", "scripts/install-checkin-task.ps1",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
}

func TestWorkBuddyCheckinScripts(t *testing.T) {
	main, install := WorkBuddyCheckinScripts()
	if len(main) == 0 || len(install) == 0 {
		t.Fatal("embedded check-in scripts are empty")
	}
	if !bytes.Contains(main, []byte("checkin task start")) {
		t.Fatalf("main script does not look like the check-in runner")
	}
	if !bytes.Contains(install, []byte("WorkBuddy Daily Checkin")) {
		t.Fatalf("install script does not contain the task name")
	}
}
