package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/logx"
)

// ImportResult reports what a DSH import did. Tokens are never included.
type ImportResult struct {
	Files    []string      `json:"files"`
	Imported int           `json:"imported"`
	Skipped  int           `json:"skipped"`
	Errors   []string      `json:"errors,omitempty"`
	Accounts []AccountView `json:"accounts"`
}

type dshLoginStore struct {
	Version  int               `json:"version"`
	ActiveID string            `json:"activeId"`
	Sessions []dshLoginSession `json:"sessions"`
}

type dshLoginSession struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	CreatedAt int64      `json:"createdAt"`
	UpdatedAt int64      `json:"updatedAt"`
	Auth      dshAuth    `json:"auth"`
	Account   dshAccount `json:"account"`
}

type dshAuth struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	TokenType        string `json:"tokenType"`
	SessionState     string `json:"sessionState"`
	Scope            string `json:"scope"`
	Domain           string `json:"domain"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	ExpiresAt        int64  `json:"expiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
}

type dshAccount struct {
	UserID       string `json:"userId"`
	Uin          string `json:"uin"`
	Email        string `json:"email"`
	Type         string `json:"type"`
	DisplayName  string `json:"displayName"`
	EnterpriseID string `json:"enterpriseId"`
}

// ImportDSH scans `.dsh/.credentials.yaml` files and imports every WorkBuddy
// login session. explicit may be a file or a directory; empty triggers the
// standard locations (including every Windows user profile).
func (m *Manager) ImportDSH(explicit string) (ImportResult, error) {
	var out ImportResult
	paths, err := discoverDSHPaths(explicit)
	if err != nil {
		return out, err
	}
	out.Files = paths
	if len(paths) == 0 {
		return out, errors.New("未找到 DSH 凭据文件（可在请求里指定 path）")
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("%s：读取失败（%s）", p, err))
			continue
		}
		value, ok := extractYAMLScalar(string(raw), "WORKBUDDY_LOGIN_SESSIONS")
		if !ok {
			value, ok = extractYAMLScalar(string(raw), "WORKBUDDY_LOGIN_SESSION")
		}
		if !ok {
			out.Errors = append(out.Errors, fmt.Sprintf("%s：没有 WorkBuddy 登录凭据", p))
			continue
		}
		var store dshLoginStore
		if err := json.Unmarshal([]byte(value), &store); err != nil {
			// A single-session record has no "sessions" wrapper.
			var single dshLoginSession
			if err2 := json.Unmarshal([]byte(value), &single); err2 != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s：凭据 JSON 无法解析", p))
				continue
			}
			store.Sessions = []dshLoginSession{single}
		}
		if len(store.Sessions) == 0 {
			out.Errors = append(out.Errors, fmt.Sprintf("%s：凭据里没有会话", p))
			continue
		}
		for _, s := range store.Sessions {
			acc, ok := accountFromDSH(s)
			if !ok {
				out.Skipped++
				continue
			}
			if _, err := m.Upsert(acc); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s：保存失败（%s）", p, err))
				continue
			}
			out.Imported++
		}
	}
	out.Accounts = m.Snapshot()
	m.logInfo("workbuddy_dsh_import", map[string]any{
		"files": len(out.Files), "imported": out.Imported, "skipped": out.Skipped,
	})
	return out, nil
}

func accountFromDSH(s dshLoginSession) (Account, bool) {
	if strings.TrimSpace(s.Auth.AccessToken) == "" || strings.TrimSpace(s.Auth.RefreshToken) == "" {
		return Account{}, false
	}
	region := ClassifyDomain(s.Auth.Domain)
	now := time.Now().UnixMilli()
	expiresAt := s.Auth.ExpiresAt
	if expiresAt == 0 && s.Auth.ExpiresIn > 0 {
		expiresAt = now + s.Auth.ExpiresIn*1000
	}
	refreshExpiresAt := s.Auth.RefreshExpiresAt
	if refreshExpiresAt == 0 && s.Auth.RefreshExpiresIn > 0 {
		refreshExpiresAt = now + s.Auth.RefreshExpiresIn*1000
	}
	label := strings.TrimSpace(s.Account.DisplayName)
	if label == "" {
		label = strings.TrimSpace(s.Label)
	}
	if label == "" {
		label = strings.TrimSpace(s.Account.Email)
	}
	if label == "" {
		label = strings.TrimSpace(s.Account.UserID)
	}
	id := strings.TrimSpace(s.ID)
	if id == "" {
		id = accountID(s.Account.UserID, s.Account.Email, s.Auth.RefreshToken)
	}
	return Account{
		ID: id, Label: label, Region: region, Domain: s.Auth.Domain,
		UserID: s.Account.UserID, EnterpriseID: s.Account.EnterpriseID,
		Uin: s.Account.Uin, Email: s.Account.Email, Type: s.Account.Type,
		AccessToken: s.Auth.AccessToken, RefreshToken: s.Auth.RefreshToken,
		ExpiresAt: expiresAt, RefreshExpiresAt: refreshExpiresAt,
		Status: "ok", UpdatedAt: now,
	}, true
}

func discoverDSHPaths(explicit string) ([]string, error) {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			p = filepath.Join(p, ".credentials.yaml")
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			out = append(out, p)
		}
	}
	if explicit != "" {
		add(explicit)
		return dedupeStrings(out), nil
	}
	if v := os.Getenv("WORKBUDDY_DSH_CREDENTIALS"); v != "" {
		add(v)
	}
	if runtime.GOOS == "windows" {
		if matches, err := filepath.Glob(`C:\Users\*\.dsh\.credentials.yaml`); err == nil {
			for _, p := range matches {
				add(p)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".dsh", ".credentials.yaml"))
	}
	return dedupeStrings(out), nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// extractYAMLScalar pulls one top-level scalar value out of a YAML document
// without parsing unrelated secrets. DSH writes the WorkBuddy store as a
// single-quoted one-line JSON scalar; multi-line quoted scalars are handled as
// a best effort so a future formatting change does not silently break import.
func extractYAMLScalar(raw, key string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimLeft(lines[i], " \t")
		if !strings.HasPrefix(line, key+":") {
			continue
		}
		rest := strings.TrimSpace(line[len(key)+1:])
		if rest == "" {
			return "", false
		}
		switch rest[0] {
		case '\'':
			return readQuotedScalar(lines, i, rest, '\'')
		case '"':
			return readQuotedScalar(lines, i, rest, '"')
		case '|', '>':
			// Block scalars are not expected for this key; join indented lines.
			var b strings.Builder
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				if len(lines[j]) > 0 && lines[j][0] != ' ' && lines[j][0] != '\t' {
					break
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(strings.TrimSpace(lines[j]))
			}
			return b.String(), b.Len() > 0
		default:
			if idx := strings.Index(rest, " #"); idx >= 0 {
				rest = strings.TrimSpace(rest[:idx])
			}
			return rest, rest != ""
		}
	}
	return "", false
}

func readQuotedScalar(lines []string, startLine int, first string, quote byte) (string, bool) {
	var b strings.Builder
	segment := first[1:]
	pendingBlank := 0
	wrote := false
	for i := startLine; i < len(lines); i++ {
		if i > startLine {
			// YAML flow scalars fold a single line break into a space and
			// strip indentation/trailing spaces. This matters: DSH writes the
			// WorkBuddy JSON across several physical lines, and keeping "\n"
			// would produce invalid JSON.
			segment = strings.TrimSpace(lines[i])
			if segment == "" {
				pendingBlank++
				continue
			}
			if wrote {
				if pendingBlank > 0 {
					b.WriteString(strings.Repeat("\n", pendingBlank))
					pendingBlank = 0
				} else {
					b.WriteByte(' ')
				}
			}
		}
		consumed, closed := consumeQuoted(segment, quote)
		b.WriteString(consumed)
		if consumed != "" {
			wrote = true
		}
		if closed {
			return b.String(), true
		}
	}
	return "", false
}

// consumeQuoted returns the unescaped content before the closing quote.
func consumeQuoted(s string, quote byte) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String(), true
			}
			b.WriteByte(c)
			continue
		}
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		if c == '"' {
			return b.String(), true
		}
		b.WriteByte(c)
	}
	return b.String(), false
}

// LogImportError is a tiny helper used by the admin layer to keep errors
// readable without leaking file contents.
func LogImportError(log *logx.Logger, msg string) {
	if log != nil {
		log.Warn("workbuddy_dsh_import_error", map[string]any{"error": logx.Redact(msg, 300)})
	}
}
