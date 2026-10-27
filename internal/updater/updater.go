package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/api"
	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

// PublicKeyBase64 is replaced at release time / by installer config.
var PublicKeyBase64 = defaultPublicKey

type Manifest struct {
	Version   string `json:"version"`
	Asset     string `json:"asset"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
	Notes     string `json:"notes"`
}

type Manager struct {
	Home        string
	Version     string
	ManifestURL string
	PublicKey   string
	Client      *http.Client
	Log         *logx.Logger
	ApplyFunc   func(path string) error

	mu        sync.Mutex
	cached    api.UpdateStatus
	cachedAt  time.Time
	cachedErr error
}

func New(cfg *config.Config, version string, log *logx.Logger, apply func(string) error) *Manager {
	pub := cfg.Update.PublicKey
	if pub == "" {
		pub = PublicKeyBase64
	}
	return &Manager{
		Home: cfg.Home, Version: version, ManifestURL: cfg.Update.ManifestURL,
		PublicKey: pub, Log: log, ApplyFunc: apply,
		Client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (m *Manager) Enabled() bool { return m.ManifestURL != "" }

func (m *Manager) Cached() api.UpdateStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cachedAt.IsZero() {
		return api.UpdateStatus{Current: m.Version}
	}
	st := m.cached
	st.Error = ""
	if m.cachedErr != nil {
		st.Error = m.cachedErr.Error()
	}
	return st
}

func (m *Manager) Check(ctx context.Context) (api.UpdateStatus, error) {
	m.mu.Lock()
	if !m.cachedAt.IsZero() && time.Since(m.cachedAt) < 10*time.Minute {
		st, err := m.cached, m.cachedErr
		m.mu.Unlock()
		return st, err
	}
	m.mu.Unlock()
	st, err := m.check(ctx)
	m.mu.Lock()
	m.cached, m.cachedAt, m.cachedErr = st, time.Now(), err
	m.mu.Unlock()
	return st, err
}

func (m *Manager) check(ctx context.Context) (api.UpdateStatus, error) {
	out := api.UpdateStatus{Current: m.Version}
	if m.ManifestURL == "" {
		return out, errors.New("manifest URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.ManifestURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", "opencode-model-gateway/"+m.Version)
	resp, err := m.Client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("manifest HTTP %d", resp.StatusCode)
	}
	var man Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&man); err != nil {
		return out, err
	}
	out.Latest = man.Version
	out.URL = man.Asset
	out.Notes = man.Notes
	out.Available = versionGreater(man.Version, m.Version)
	return out, nil
}

func (m *Manager) Apply(ctx context.Context) (api.UpdateStatus, error) {
	st, err := m.Check(ctx)
	if err != nil {
		return st, err
	}
	if !st.Available {
		return st, nil
	}
	// Re-fetch manifest; Check only returned status.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.ManifestURL, nil)
	if err != nil {
		return st, err
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return st, err
	}
	var man Manifest
	err = json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&man)
	_ = resp.Body.Close()
	if err != nil {
		return st, err
	}
	if err := verifyManifest(man, m.PublicKey); err != nil {
		return st, err
	}
	dl, err := http.NewRequestWithContext(ctx, http.MethodGet, man.Asset, nil)
	if err != nil {
		return st, err
	}
	dl.Header.Set("User-Agent", "opencode-model-gateway/"+m.Version)
	ar, err := m.Client.Do(dl)
	if err != nil {
		return st, err
	}
	defer ar.Body.Close()
	if ar.StatusCode != http.StatusOK {
		return st, fmt.Errorf("asset HTTP %d", ar.StatusCode)
	}
	if err := os.MkdirAll(filepath.Join(m.Home, "updates"), 0o755); err != nil {
		return st, err
	}
	dst := filepath.Join(m.Home, "updates", "gateway-"+man.Version+".exe")
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return st, err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(ar.Body, 100*1024*1024))
	_ = f.Close()
	if err != nil {
		return st, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, man.SHA256) {
		_ = os.Remove(tmp)
		return st, fmt.Errorf("sha256 mismatch: got %s", got)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return st, err
	}
	if m.ApplyFunc == nil {
		return st, errors.New("update apply is not supported on this platform")
	}
	if err := m.ApplyFunc(dst); err != nil {
		return st, err
	}
	return st, nil
}

func verifyManifest(m Manifest, publicKey string) error {
	if publicKey == "" {
		return errors.New("update public key is empty")
	}
	raw, err := decodeKey(publicKey)
	if err != nil {
		return err
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("public key length %d", len(raw))
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return err
	}
	msg := []byte(m.Version + "\n" + strings.ToLower(m.SHA256) + "\n" + m.Asset)
	if !ed25519.Verify(ed25519.PublicKey(raw), msg, sig) {
		return errors.New("invalid update signature")
	}
	return nil
}

func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("public key is not base64/hex")
}

func versionGreater(a, b string) bool {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func parseVersion(v string) []int {
	v = strings.TrimPrefix(v, "v")
	v = strings.SplitN(v, "-", 2)[0]
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		fmt.Sscanf(p, "%d", &n)
		out = append(out, n)
	}
	return out
}
