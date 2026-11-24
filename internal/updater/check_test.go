package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/api"
)

func TestCheckAndApply(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	asset := []byte("new gateway bytes")
	sum := sha256.Sum256(asset)
	sha := hex.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			msg := []byte("1.1.0\n" + sha + "\n" + srv.URL + "/gateway.exe")
			m := Manifest{Version: "1.1.0", Asset: srv.URL + "/gateway.exe", SHA256: sha, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))}
			_ = json.NewEncoder(w).Encode(m)
		case "/gateway.exe":
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := t.TempDir()
	got := ""
	m := &Manager{
		Home: home, Version: "1.0.0", ManifestURL: srv.URL + "/latest.json",
		PublicKey: base64.StdEncoding.EncodeToString(pub), Client: http.DefaultClient,
		ApplyFunc: func(path string) error {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			got = string(b)
			return nil
		},
	}
	st, err := m.Check(context.Background())
	if err != nil || !st.Available || st.Latest != "1.1.0" {
		t.Fatalf("check: %+v err=%v", st, err)
	}
	if _, err := m.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != string(asset) {
		t.Fatalf("applied content %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, "updates", "gateway-1.1.0.exe")); err != nil {
		t.Fatal(err)
	}
}

func TestApplyProgress(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	asset := bytes.Repeat([]byte("x"), 256*1024)
	sum := sha256.Sum256(asset)
	sha := hex.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			msg := []byte("1.1.0\n" + sha + "\n" + srv.URL + "/gateway.exe")
			m := Manifest{Version: "1.1.0", Asset: srv.URL + "/gateway.exe", SHA256: sha, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))}
			_ = json.NewEncoder(w).Encode(m)
		case "/gateway.exe":
			w.Header().Set("Content-Length", strconv.Itoa(len(asset)))
			for i := 0; i < len(asset); i += 32 * 1024 {
				end := i + 32*1024
				if end > len(asset) {
					end = len(asset)
				}
				if _, err := w.Write(asset[i:end]); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	var atApply api.UpdateProgress
	m := &Manager{
		Home: home, Version: "1.0.0", ManifestURL: srv.URL + "/latest.json",
		PublicKey: base64.StdEncoding.EncodeToString(pub), Client: http.DefaultClient,
	}
	m.ApplyFunc = func(path string) error {
		atApply = m.Progress()
		return nil
	}
	if _, err := m.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := m.Progress()
	if p.Phase != "applying" || !p.Running {
		t.Fatalf("progress = %+v", p)
	}
	if p.DownloadedBytes != int64(len(asset)) || p.TotalBytes != int64(len(asset)) || p.Percent != 100 {
		t.Fatalf("download progress = %+v", p)
	}
	if atApply.Phase != "applying" || atApply.DownloadedBytes != int64(len(asset)) {
		t.Fatalf("progress at apply = %+v", atApply)
	}
	if _, err := os.Stat(filepath.Join(home, "updates", progressFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFailureProgress(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(Manifest{
			Version: "1.1.0", Asset: "http://example.invalid/gateway.exe",
			SHA256: "00", Signature: "bad",
		})
	}))
	defer srv.Close()

	m := &Manager{
		Home: t.TempDir(), Version: "1.0.0", ManifestURL: srv.URL + "/latest.json",
		PublicKey: base64.StdEncoding.EncodeToString(pub), Client: http.DefaultClient,
	}
	if _, err := m.Apply(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	p := m.Progress()
	if p.Phase != "failed" || p.Running || p.Error == "" {
		t.Fatalf("progress = %+v", p)
	}
}

func TestCheckRejectsUnsigned(t *testing.T) {
	m := &Manager{Version: "1.0.0", ManifestURL: "http://example.invalid/latest.json", Client: http.DefaultClient}
	if _, err := m.Check(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

var _ = io.EOF
