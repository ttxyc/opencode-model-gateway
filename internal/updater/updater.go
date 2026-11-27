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
	"net/url"
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

	progressMu sync.Mutex
	progress   api.UpdateProgress
	applyMu    sync.Mutex
}

func New(cfg *config.Config, version string, log *logx.Logger, apply func(string) error) *Manager {
	pub := cfg.Update.PublicKey
	if pub == "" {
		pub = PublicKeyBase64
	}
	return &Manager{
		Home: cfg.Home, Version: version, ManifestURL: cfg.Update.ManifestURL,
		PublicKey: pub, Log: log, ApplyFunc: apply,
		Client: &http.Client{Timeout: 5 * time.Minute, Transport: newProxyFallbackTransport()},
	}
}

// proxyFallbackTransport prefers the environment proxy but falls back to a
// direct connection when the proxy itself is unreachable. The Windows service
// pins HTTPS_PROXY to a local v2rayN/Clash port; if that client is not running,
// update requests would otherwise fail with "proxyconnect ... connection
// refused" even when direct GitHub access works.
type proxyFallbackTransport struct {
	proxied http.RoundTripper
	direct  http.RoundTripper
}

func newProxyFallbackTransport() http.RoundTripper {
	direct := http.DefaultTransport.(*http.Transport).Clone()
	direct.Proxy = nil
	return &proxyFallbackTransport{proxied: http.DefaultTransport, direct: direct}
}

func (t *proxyFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.proxied.RoundTrip(req)
	if err == nil || !isProxyDialError(err) {
		return resp, err
	}
	retry := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			return nil, err
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, err
		}
		retry.Body = body
	}
	resp, directErr := t.direct.RoundTrip(retry)
	if directErr != nil {
		return nil, fmt.Errorf("proxy unavailable (%v); direct retry failed: %w", err, directErr)
	}
	return resp, nil
}

func isProxyDialError(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return strings.Contains(err.Error(), "proxyconnect")
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

const progressFileName = "apply-progress.json"

// Progress returns the live update phase. If the process was restarted during
// an update, the helper's on-disk progress file is used so the admin page can
// still show whether the update finished or rolled back.
func (m *Manager) Progress() api.UpdateProgress {
	m.progressMu.Lock()
	p := m.progress
	m.progressMu.Unlock()
	if p.Running || (p.Phase != "" && p.Phase != "idle") {
		return p
	}
	if fp, ok := m.readProgressFile(); ok {
		return fp
	}
	if p.Phase == "" {
		p.Phase = "idle"
		p.Message = "空闲"
	}
	return p
}

func (m *Manager) setProgress(phase string, running bool, message, version string, downloaded, total int64, errText string) {
	now := time.Now()
	m.progressMu.Lock()
	if m.progress.StartedAt == nil && phase != "idle" {
		m.progress.StartedAt = &now
	}
	p := m.progress
	p.Running = running
	p.Phase = phase
	p.Version = version
	p.TotalBytes = total
	p.DownloadedBytes = downloaded
	p.Message = message
	p.Error = errText
	p.UpdatedAt = &now
	if total > 0 {
		pct := float64(downloaded) * 100 / float64(total)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		p.Percent = float64(int(pct*100)) / 100
	} else {
		p.Percent = 0
	}
	m.progress = p
	m.progressMu.Unlock()
	m.writeProgressFile(p)
}

func (m *Manager) readProgressFile() (api.UpdateProgress, bool) {
	if m.Home == "" {
		return api.UpdateProgress{}, false
	}
	raw, err := os.ReadFile(filepath.Join(m.Home, "updates", progressFileName))
	if err != nil {
		return api.UpdateProgress{}, false
	}
	var p api.UpdateProgress
	if json.Unmarshal(raw, &p) != nil || p.UpdatedAt == nil {
		return api.UpdateProgress{}, false
	}
	if p.StartedAt != nil && p.StartedAt.IsZero() {
		p.StartedAt = nil
	}
	if time.Since(*p.UpdatedAt) > 15*time.Minute {
		return api.UpdateProgress{}, false
	}
	if p.Running && time.Since(*p.UpdatedAt) > 5*time.Minute {
		p.Running = false
		p.Phase = "failed"
		p.Message = "上次更新未完成"
		p.Error = "更新状态超时，请查看网关日志"
	}
	return p, true
}

func (m *Manager) writeProgressFile(p api.UpdateProgress) {
	if m.Home == "" {
		return
	}
	dir := filepath.Join(m.Home, "updates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, progressFileName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, progressFileName))
}

func (m *Manager) Check(ctx context.Context) (api.UpdateStatus, error) {
	m.mu.Lock()
	// Cache successes for 10 minutes; always retry after a failure so the
	// admin "检查更新" button works as soon as the proxy/network is restored.
	if !m.cachedAt.IsZero() && m.cachedErr == nil && time.Since(m.cachedAt) < 10*time.Minute {
		st := m.cached
		m.mu.Unlock()
		return st, nil
	}
	m.mu.Unlock()
	st, err := m.check(ctx)
	m.mu.Lock()
	m.cached, m.cachedAt, m.cachedErr = st, time.Now(), err
	m.mu.Unlock()
	return st, err
}

// ForceCheck bypasses the 10-minute result cache used by background checks.
// The admin page uses it so "检查更新" always sees a just-published release.
func (m *Manager) ForceCheck(ctx context.Context) (api.UpdateStatus, error) {
	m.mu.Lock()
	m.cachedAt = time.Time{}
	m.cachedErr = nil
	m.mu.Unlock()
	return m.Check(ctx)
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
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	if p := m.Progress(); p.Running {
		return api.UpdateStatus{Current: m.Version}, errors.New("更新已在进行中")
	}
	m.progressMu.Lock()
	m.progress = api.UpdateProgress{}
	m.progressMu.Unlock()
	m.setProgress("checking", true, "检查更新…", "", 0, 0, "")

	st, err := m.Check(ctx)
	if err != nil {
		m.setProgress("failed", false, "检查更新失败", "", 0, 0, err.Error())
		return st, err
	}
	if !st.Available {
		m.setProgress("idle", false, "已是最新版本", st.Current, 0, 0, "")
		return st, nil
	}
	// Re-fetch manifest; Check only returned status.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.ManifestURL, nil)
	if err != nil {
		m.setProgress("failed", false, "读取更新清单失败", st.Latest, 0, 0, err.Error())
		return st, err
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		m.setProgress("failed", false, "读取更新清单失败", st.Latest, 0, 0, err.Error())
		return st, err
	}
	var man Manifest
	err = json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&man)
	_ = resp.Body.Close()
	if err != nil {
		m.setProgress("failed", false, "解析更新清单失败", st.Latest, 0, 0, err.Error())
		return st, err
	}
	if err := verifyManifest(man, m.PublicKey); err != nil {
		m.setProgress("failed", false, "更新签名校验失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	dl, err := http.NewRequestWithContext(ctx, http.MethodGet, man.Asset, nil)
	if err != nil {
		m.setProgress("failed", false, "创建下载请求失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	dl.Header.Set("User-Agent", "opencode-model-gateway/"+m.Version)
	ar, err := m.Client.Do(dl)
	if err != nil {
		m.setProgress("failed", false, "下载更新失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	defer ar.Body.Close()
	if ar.StatusCode != http.StatusOK {
		err := fmt.Errorf("asset HTTP %d", ar.StatusCode)
		m.setProgress("failed", false, "下载更新失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	if err := os.MkdirAll(filepath.Join(m.Home, "updates"), 0o755); err != nil {
		m.setProgress("failed", false, "创建更新目录失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	dst := filepath.Join(m.Home, "updates", "gateway-"+man.Version+".exe")
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		m.setProgress("failed", false, "创建更新文件失败", man.Version, 0, 0, err.Error())
		return st, err
	}
	total := ar.ContentLength
	if total < 0 {
		total = 0
	}
	h := sha256.New()
	pr := &progressReader{r: io.LimitReader(ar.Body, 100*1024*1024), m: m, version: man.Version, total: total}
	_, err = io.Copy(io.MultiWriter(f, h), pr)
	_ = f.Close()
	if err != nil {
		m.setProgress("failed", false, "下载更新失败", man.Version, pr.read, pr.total, err.Error())
		return st, err
	}
	m.setProgress("verifying", true, "校验文件哈希…", man.Version, pr.read, pr.total, "")
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, man.SHA256) {
		_ = os.Remove(tmp)
		err := fmt.Errorf("sha256 mismatch: got %s", got)
		m.setProgress("failed", false, "更新文件哈希不匹配", man.Version, pr.read, pr.total, err.Error())
		return st, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		m.setProgress("failed", false, "保存更新文件失败", man.Version, pr.read, pr.total, err.Error())
		return st, err
	}
	if m.ApplyFunc == nil {
		err := errors.New("update apply is not supported on this platform")
		m.setProgress("failed", false, "当前平台不支持自动更新", man.Version, pr.read, pr.total, err.Error())
		return st, err
	}
	m.setProgress("applying", true, "启动更新助手，服务即将重启…", man.Version, pr.read, pr.total, "")
	if err := m.ApplyFunc(dst); err != nil {
		m.setProgress("failed", false, "应用更新失败", man.Version, pr.read, pr.total, err.Error())
		return st, err
	}
	m.setProgress("applying", true, "更新助手已启动，等待服务重启…", man.Version, pr.read, pr.total, "")
	return st, nil
}

type progressReader struct {
	r       io.Reader
	m       *Manager
	version string
	total   int64
	read    int64
	last    time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.last.IsZero() || time.Since(p.last) >= 200*time.Millisecond || err != nil {
		p.m.setProgress("downloading", true, "下载 gateway.exe…", p.version, p.read, p.total, "")
		p.last = time.Now()
	}
	return n, err
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
