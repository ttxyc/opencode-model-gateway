package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	ServiceName = "opencode-model-gateway"
	DefaultPort = 8788
)

type Provider struct {
	ID      string   `json:"-"`
	Label   string   `json:"label"`
	BaseURL string   `json:"baseURL"`
	Models  []string `json:"models"`
	Kind    string   `json:"kind,omitempty"`
}

type Limits struct {
	MaxBodyBytes     int64 `json:"maxBodyBytes"`
	MaxConcurrent    int   `json:"maxConcurrent"`
	HeadersTimeoutMS int64 `json:"headersTimeoutMs"`
	TotalTimeoutMS   int64 `json:"totalTimeoutMs"`
	MaxRetries       int   `json:"maxRetries"`
}

type LogConfig struct {
	Dir      string `json:"dir"`
	MaxBytes int64  `json:"maxBytes"`
	MaxFiles int    `json:"maxFiles"`
}

type UpdateConfig struct {
	Enabled      bool   `json:"enabled"`
	ManifestURL  string `json:"manifestUrl"`
	PublicKey    string `json:"publicKey"`
	CheckHours   int    `json:"checkHours"`
	AllowRestart bool   `json:"allowRestart"`
}

type Paths struct {
	Home    string
	Config  string
	Keys    string
	State   string
	Logs    string
	Updates string
}

type Config struct {
	Home        string               `json:"-"`
	Bind        string               `json:"bind"`
	Port        int                  `json:"port"`
	ClientToken string               `json:"token"`
	AdminToken  string               `json:"adminToken"`
	Providers   map[string]*Provider `json:"providers"`
	Aliases     map[string]string    `json:"aliases"`
	Limits      Limits               `json:"limits"`
	Log         LogConfig            `json:"log"`
	Update      UpdateConfig         `json:"update"`
	Paths       Paths                `json:"-"`
}

func DefaultHome() string {
	if v := strings.TrimSpace(os.Getenv("GATEWAY_HOME")); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, ServiceName)
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", ServiceName)
}

func DefaultPaths(home string) Paths {
	return Paths{
		Home:    home,
		Config:  filepath.Join(home, "config", "gateway.json"),
		Keys:    filepath.Join(home, "keys", "quota-line.json"),
		State:   filepath.Join(home, "state", "key-router.json"),
		Logs:    filepath.Join(home, "logs"),
		Updates: filepath.Join(home, "updates"),
	}
}

func (c *Config) Normalize() {
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Limits.MaxBodyBytes <= 0 {
		c.Limits.MaxBodyBytes = 64 * 1024 * 1024
	}
	if c.Limits.MaxConcurrent <= 0 {
		c.Limits.MaxConcurrent = 64
	}
	if c.Limits.HeadersTimeoutMS <= 0 {
		c.Limits.HeadersTimeoutMS = 300000
	}
	if c.Limits.TotalTimeoutMS <= 0 {
		c.Limits.TotalTimeoutMS = 900000
	}
	if c.Limits.MaxRetries <= 0 {
		c.Limits.MaxRetries = 2
	}
	if c.Log.MaxBytes <= 0 {
		c.Log.MaxBytes = 10 * 1024 * 1024
	}
	if c.Log.MaxFiles <= 0 {
		c.Log.MaxFiles = 5
	}
	if c.Log.Dir == "" {
		c.Log.Dir = c.Paths.Logs
	}
	if c.Update.CheckHours <= 0 {
		c.Update.CheckHours = 24
	}
	if c.Update.ManifestURL == "" {
		c.Update.Enabled = false
	}
	if c.Update.ManifestURL == "" {
		c.Update.ManifestURL = "https://github.com/ttxyc/opencode-model-gateway/releases/latest/download/latest.json"
	}
	if c.AdminToken == "" {
		c.AdminToken = c.ClientToken
	}
	for id, p := range c.Providers {
		p.ID = id
		if p.Kind == "" {
			p.Kind = id
		}
		if p.Label == "" {
			p.Label = id
		}
	}
}

func Load(home string) (*Config, error) {
	if home == "" {
		home = DefaultHome()
	}
	paths := DefaultPaths(home)
	if v := os.Getenv("GATEWAY_CONFIG"); v != "" {
		paths.Config = v
	}
	if v := os.Getenv("KEY_ROUTER_CONFIG"); v != "" {
		paths.Keys = v
	}
	if v := os.Getenv("KEY_ROUTER_STATE"); v != "" {
		paths.State = v
	}
	if v := os.Getenv("GATEWAY_LOG_DIR"); v != "" {
		paths.Logs = v
	}
	raw, err := os.ReadFile(paths.Config)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", paths.Config, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", paths.Config, err)
	}
	cfg.Home = home
	cfg.Paths = paths
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for _, dir := range []string{
		filepath.Dir(paths.Config), filepath.Dir(paths.Keys), filepath.Dir(paths.State),
		paths.Logs, paths.Updates,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.ClientToken) == "" {
		return errors.New("token is required")
	}
	if len(c.ClientToken) < 16 {
		return errors.New("token must be at least 16 characters")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if len(c.Providers) == 0 {
		return errors.New("providers is empty")
	}
	for id, p := range c.Providers {
		if id == "" || strings.ContainsAny(id, "/\\") {
			return fmt.Errorf("invalid provider id %q", id)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %s has invalid baseURL %q", id, p.BaseURL)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	return nil
}

func (c *Config) ResolveModel(providerID, model string, unified bool) (string, string, error) {
	if strings.TrimSpace(model) == "" {
		return "", "", errors.New("model is required")
	}
	if unified {
		if target, ok := c.Aliases[model]; ok {
			model = target
		}
		idx := strings.Index(model, "/")
		if idx <= 0 || idx == len(model)-1 {
			return "", "", errors.New("model must be '<provider>/<model-id>'")
		}
		pid := model[:idx]
		if _, ok := c.Providers[pid]; !ok {
			return "", "", fmt.Errorf("unknown provider prefix: %s", pid)
		}
		return pid, model[idx+1:], nil
	}
	if _, ok := c.Providers[providerID]; !ok {
		return "", "", fmt.Errorf("unknown provider: %s", providerID)
	}
	m := model
	prefix := providerID + "/"
	if strings.HasPrefix(m, prefix) {
		m = strings.TrimPrefix(m, prefix)
	}
	if m == "" {
		return "", "", errors.New("model is empty")
	}
	return providerID, m, nil
}

func (c *Config) UnifiedModels() []map[string]string {
	var out []map[string]string
	for id, p := range c.Providers {
		for _, m := range p.Models {
			out = append(out, map[string]string{"id": id + "/" + m, "provider": id})
		}
	}
	for alias, target := range c.Aliases {
		pid := target
		if i := strings.Index(target, "/"); i > 0 {
			pid = target[:i]
		}
		out = append(out, map[string]string{"id": alias, "provider": pid})
	}
	return out
}

func (c *Config) ProviderModels(id string) ([]string, bool) {
	p, ok := c.Providers[id]
	if !ok {
		return nil, false
	}
	return p.Models, true
}
