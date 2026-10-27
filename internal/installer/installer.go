package installer

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed assets
var assets embed.FS

// Prepare extracts the installer assets and a copy of the running executable
// into a per-run temp directory. The layout matches the release package:
//
//	gateway.exe
//	config.example.json
//	publickey.txt
//	opencode-gateway.vbs
//	scripts/install.ps1
//	scripts/install-wsl.ps1
func Prepare(executable string) (string, error) {
	dir, err := os.MkdirTemp("", "opencode-gateway-install-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(assets, "assets", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(path, "assets/")
		if rel == "" || rel == "assets" {
			return nil
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, readErr := assets.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			return mkErr
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := copyExecutable(executable, filepath.Join(dir, "gateway.exe")); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func ScriptPath(dir string) string {
	return filepath.Join(dir, "scripts", "install.ps1")
}

func Cleanup(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open installer executable: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
