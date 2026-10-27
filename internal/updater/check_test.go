package updater

import (
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
	"testing"
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

func TestCheckRejectsUnsigned(t *testing.T) {
	m := &Manager{Version: "1.0.0", ManifestURL: "http://example.invalid/latest.json", Client: http.DefaultClient}
	if _, err := m.Check(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

var _ = io.EOF
