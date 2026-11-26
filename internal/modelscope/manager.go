package modelscope

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	legacyDir = `D:\wsl-opencode\modelscope-auto`
	todayFmt  = "2006-01-02"
	timeFmt   = "2006-01-02 15:04:05"
)

type Account struct {
	Name     string `json:"name"`
	MsName   string `json:"msName,omitempty"`
	Cookie   string `json:"cookie,omitempty"`
	Password string `json:"password,omitempty"`
	Article  string `json:"article,omitempty"`
	Enabled  bool   `json:"enabled"`
	Note     string `json:"note,omitempty"`
	CookieAt int64  `json:"cookieAt,omitempty"`
}

type AccountView struct {
	Name    string `json:"name"`
	MsName  string `json:"msName,omitempty"`
	Enabled bool   `json:"enabled"`
	Done    bool   `json:"done"`
}

type dailyState struct {
	Date     string   `json:"date"`
	DoneAt   string   `json:"doneAt"`
	Accounts []string `json:"accounts"`
}

type storeFile struct {
	Version     int        `json:"version"`
	Accounts    []Account  `json:"accounts"`
	Daily       dailyState `json:"daily"`
	ArticlePool []string   `json:"articlePool,omitempty"`
}

type Progress struct {
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Current   string `json:"current,omitempty"`
	Phase     string `json:"phase,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type Action struct {
	Action     string    `json:"action"`
	Running    bool      `json:"running"`
	LastExit   int       `json:"lastExit"`
	StartedAt  string    `json:"startedAt,omitempty"`
	FinishedAt string    `json:"finishedAt,omitempty"`
	Progress   *Progress `json:"progress,omitempty"`
}

type AutoTask struct {
	Exists     bool   `json:"exists"`
	State      string `json:"state,omitempty"`
	LastRun    string `json:"lastRun,omitempty"`
	LastResult int    `json:"lastResult,omitempty"`
	NextRun    string `json:"nextRun,omitempty"`
}

type Status struct {
	Dir          string        `json:"dir"`
	Today        string        `json:"today"`
	Accounts     []AccountView `json:"accounts"`
	EnabledCount int           `json:"enabledCount"`
	DoneToday    bool          `json:"doneToday"`
	DoneAt       string        `json:"doneAt,omitempty"`
	LogFile      string        `json:"logFile,omitempty"`
	LogTail      string        `json:"logTail"`
	AutoTask     AutoTask      `json:"autoTask"`
	Run          Action        `json:"run"`
	Refresh      Action        `json:"refresh"`
	Error        string        `json:"error,omitempty"`
}

type LoginFunc func(ctx context.Context, account Account) (string, error)

type Manager struct {
	mu        sync.Mutex
	statePath string
	logPath   string
	store     storeFile
	run       Action
	refresh   Action
	client    *apiClient
	login     LoginFunc
}

func NewManager(stateDir, logDir string) *Manager {
	m := &Manager{
		statePath: filepath.Join(stateDir, "modelscope.json"),
		logPath:   filepath.Join(logDir, "modelscope.log"),
		client:    newAPIClient(),
	}
	m.run = Action{Action: "run"}
	m.refresh = Action{Action: "refresh"}
	if !m.load() {
		m.importLegacy()
	}
	return m
}

func (m *Manager) load() bool {
	b, err := os.ReadFile(m.statePath)
	if err != nil {
		return false
	}
	var s storeFile
	if json.Unmarshal(b, &s) != nil {
		return false
	}
	if s.Version == 0 {
		s.Version = 1
	}
	m.store = s
	return true
}

func (m *Manager) saveLocked() {
	m.store.Version = 1
	if m.store.Accounts == nil {
		m.store.Accounts = []Account{}
	}
	if m.store.ArticlePool == nil {
		m.store.ArticlePool = []string{}
	}
	b, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return
	}
	b = append(b, '\n')
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0o755)
	tmp := m.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.statePath)
	}
}

func (m *Manager) importLegacy() {
	legacy := legacyDir
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(legacy, "accounts.json"))
	if err != nil {
		return
	}
	var doc struct {
		Accounts []struct {
			Name    string `json:"name"`
			MsName  string `json:"msName"`
			Cookie  string `json:"cookie"`
			Article string `json:"article"`
			Enabled *bool  `json:"enabled"`
			Note    string `json:"note"`
		} `json:"accounts"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	password := ""
	if b, err := os.ReadFile(filepath.Join(legacy, ".secrets.json")); err == nil {
		var sec struct {
			ModelscopePassword string `json:"modelscopePassword"`
		}
		_ = json.Unmarshal(b, &sec)
		password = sec.ModelscopePassword
	}
	imported := make([]Account, 0, len(doc.Accounts))
	for _, a := range doc.Accounts {
		cookie := ""
		if a.Cookie != "" {
			if b, err := os.ReadFile(filepath.Join(legacy, filepath.FromSlash(a.Cookie))); err == nil {
				cookie = strings.TrimSpace(string(b))
			}
		}
		enabled := true
		if a.Enabled != nil {
			enabled = *a.Enabled
		}
		article := strings.TrimSpace(a.Article)
		if article == "" {
			article = "435888"
		}
		imported = append(imported, Account{
			Name: a.Name, MsName: a.MsName, Cookie: cookie, Password: password,
			Article: article, Enabled: enabled, Note: a.Note,
			CookieAt: time.Now().UnixMilli(),
		})
	}
	if len(imported) == 0 {
		return
	}
	m.store.Accounts = imported
	if b, err := os.ReadFile(filepath.Join(legacy, ".state.json")); err == nil {
		var st dailyState
		if json.Unmarshal(b, &st) == nil {
			m.store.Daily = st
		}
	}
	if b, err := os.ReadFile(filepath.Join(legacy, "articles.json")); err == nil {
		var ap struct {
			Articles []string `json:"articles"`
		}
		if json.Unmarshal(b, &ap) == nil {
			m.store.ArticlePool = ap.Articles
		}
	}
	m.saveLocked()
	m.logf("", "已从旧项目导入 %d 个账号（%s）", len(imported), legacy)
}

func (m *Manager) logf(account, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := "[" + time.Now().Format(timeFmt) + "] "
	if account != "" {
		line += "[" + account + "] "
	}
	line += msg + "\n"
	_ = os.MkdirAll(filepath.Dir(m.logPath), 0o755)
	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 8*1024*1024 {
		_ = os.Rename(m.logPath, m.logPath+".1")
		f.Close()
		f, err = os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
	}
	_, _ = f.WriteString(line)
}

func tailFile(path string, maxLines int) (string, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return path, ""
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return path, strings.Join(lines, "\n")
}

func (m *Manager) SetLoginFunc(f LoginFunc) {
	m.mu.Lock()
	m.login = f
	m.mu.Unlock()
}

func (m *Manager) LoginAccount(ctx context.Context, name string) (string, error) {
	acc, ok := m.Find(name)
	if !ok {
		return "", errors.New("账号不存在")
	}
	m.mu.Lock()
	fn := m.login
	m.mu.Unlock()
	if fn == nil {
		return "", errors.New("登录助手未配置")
	}
	m.logf(name, "需要重新登录，正在打开浏览器…")
	cookie, err := fn(ctx, acc)
	if err != nil {
		m.logf(name, "重新登录失败: %v", err)
		return "", err
	}
	loginName, err := m.fetchLoginInfo(ctx, cookie)
	if err != nil {
		m.logf(name, "登录结果无效: %v", err)
		return "", err
	}
	if acc.MsName != "" && loginName != acc.MsName {
		err := fmt.Errorf("浏览器登录的是 %s，不是 %s", loginName, acc.MsName)
		m.logf(name, "%v", err)
		return "", err
	}
	m.updateAccountCookie(name, cookie)
	m.logf(name, "重新登录成功，Cookie 已更新（登录账号 %s）", loginName)
	return loginName, nil
}

func (m *Manager) Accounts() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, len(m.store.Accounts))
	copy(out, m.store.Accounts)
	return out
}

func (m *Manager) Find(name string) (Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.store.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

func (m *Manager) Upsert(acc Account) error {
	acc.Name = strings.TrimSpace(acc.Name)
	if acc.Name == "" {
		return errors.New("账号名不能为空")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].Name == acc.Name {
			if acc.Password == "" {
				acc.Password = m.store.Accounts[i].Password
			}
			if acc.Cookie == "" {
				acc.Cookie = m.store.Accounts[i].Cookie
			}
			if acc.Article == "" {
				acc.Article = m.store.Accounts[i].Article
			}
			m.store.Accounts[i] = acc
			m.saveLocked()
			return nil
		}
	}
	if acc.Article == "" {
		acc.Article = "435888"
	}
	m.store.Accounts = append(m.store.Accounts, acc)
	m.saveLocked()
	return nil
}

func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.store.Accounts[:0]
	found := false
	for _, a := range m.store.Accounts {
		if a.Name == name {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return errors.New("账号不存在")
	}
	m.store.Accounts = out
	m.saveLocked()
	return nil
}

func (m *Manager) setProgress(done, total int, current, phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run.Progress == nil {
		m.run.Progress = &Progress{}
	}
	m.run.Progress.Total = total
	m.run.Progress.Done = done
	m.run.Progress.Current = current
	m.run.Progress.Phase = phase
	m.run.Progress.UpdatedAt = time.Now().Format(time.RFC3339)
}

func (m *Manager) setRunAction(a Action) {
	m.mu.Lock()
	m.run = a
	m.mu.Unlock()
}

func (m *Manager) setRefreshAction(a Action) {
	m.mu.Lock()
	m.refresh = a
	m.mu.Unlock()
}

func (m *Manager) Status() (Status, error) {
	m.mu.Lock()
	store := m.store
	run := m.run
	refresh := m.refresh
	m.mu.Unlock()

	today := time.Now().Format(todayFmt)
	out := Status{
		Dir:     filepath.Dir(m.statePath),
		Today:   today,
		Run:     run,
		Refresh: refresh,
		AutoTask: AutoTask{
			Exists: true, State: "Enabled", NextRun: "每天 10:00（内置）",
		},
	}
	if store.Daily.Date == today {
		out.DoneToday = true
		out.DoneAt = store.Daily.DoneAt
	}
	doneSet := map[string]bool{}
	if out.DoneToday {
		for _, n := range store.Daily.Accounts {
			doneSet[n] = true
		}
	}
	for _, a := range store.Accounts {
		if a.Enabled {
			out.EnabledCount++
		}
		out.Accounts = append(out.Accounts, AccountView{
			Name: a.Name, MsName: a.MsName, Enabled: a.Enabled, Done: doneSet[a.Name],
		})
	}
	if out.Accounts == nil {
		out.Accounts = []AccountView{}
	}
	out.LogFile, out.LogTail = tailFile(m.logPath, 160)
	return out, nil
}

func (m *Manager) StartRun(force bool) (map[string]any, error) {
	m.mu.Lock()
	if m.run.Running || m.refresh.Running {
		m.mu.Unlock()
		return nil, errors.New("ModelScope 任务正在运行中，请稍候")
	}
	started := time.Now().Format(timeFmt)
	m.run = Action{
		Action: "run", Running: true, StartedAt: started,
		Progress: &Progress{Phase: "running", UpdatedAt: time.Now().Format(time.RFC3339)},
	}
	m.mu.Unlock()

	go m.runAll(force)
	return map[string]any{
		"ok": true, "action": "run", "message": "立即运行 已触发", "startedAt": started,
	}, nil
}

func (m *Manager) StartRefresh() (map[string]any, error) {
	m.mu.Lock()
	if m.run.Running || m.refresh.Running {
		m.mu.Unlock()
		return nil, errors.New("ModelScope 任务正在运行中，请稍候")
	}
	started := time.Now().Format(timeFmt)
	m.refresh = Action{Action: "refresh", Running: true, StartedAt: started}
	m.mu.Unlock()

	go m.refreshAll()
	return map[string]any{
		"ok": true, "action": "refresh", "message": "刷新 Cookie 已触发", "startedAt": started,
	}, nil
}

func (m *Manager) updateAccountCookie(name, cookie string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].Name == name {
			m.store.Accounts[i].Cookie = cookie
			m.store.Accounts[i].CookieAt = time.Now().UnixMilli()
			m.saveLocked()
			return
		}
	}
}

func (m *Manager) updateAccountArticle(name, article string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.store.Accounts {
		if m.store.Accounts[i].Name == name {
			m.store.Accounts[i].Article = article
			m.saveLocked()
			return
		}
	}
}
