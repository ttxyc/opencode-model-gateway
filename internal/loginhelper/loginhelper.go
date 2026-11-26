package loginhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Options struct {
	Provider     string
	Account      string
	MsName       string
	LoginURL     string
	Result       string
	Profile      string
	Username     string
	PasswordFile string
	Headless     bool
	Timeout      time.Duration
}

type Result struct {
	OK         bool   `json:"ok"`
	Account    string `json:"account"`
	LoginName  string `json:"loginName,omitempty"`
	Cookie     string `json:"cookie,omitempty"`
	Error      string `json:"error,omitempty"`
	CapturedAt string `json:"capturedAt,omitempty"`
}

func Run(opts Options) error {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.LoginURL == "" {
		opts.LoginURL = "https://www.modelscope.cn/login"
	}
	res := Result{Account: opts.Account}
	err := run(&opts, &res)
	if err != nil {
		res.OK = false
		res.Error = err.Error()
	} else {
		res.OK = true
	}
	res.CapturedAt = time.Now().Format("2006-01-02 15:04:05")
	if opts.Result != "" {
		b, _ := json.Marshal(res)
		_ = os.WriteFile(opts.Result, append(b, '\n'), 0o600)
	}
	if err != nil {
		return err
	}
	return nil
}

func run(opts *Options, res *Result) error {
	if runtime.GOOS != "windows" {
		return errors.New("登录助手仅在 Windows 上可用")
	}
	browser, err := findBrowser()
	if err != nil {
		return err
	}
	profile := opts.Profile
	if profile == "" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
		}
		profile = filepath.Join(base, "opencode-model-gateway", "login-profile", opts.Provider)
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return fmt.Errorf("创建登录 profile 失败: %w", err)
	}
	_ = os.Remove(filepath.Join(profile, "DevToolsActivePort"))
	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
	}
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, opts.LoginURL)
	cmd := exec.Command(browser, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	port, browserPath, err := waitDevToolsPort(profile, 20*time.Second)
	if err != nil {
		return err
	}
	browserWS := fmt.Sprintf("ws://127.0.0.1:%d%s", port, browserPath)
	browserClient, err := dial(browserWS)
	if err != nil {
		return fmt.Errorf("连接浏览器 CDP 失败: %w", err)
	}
	defer browserClient.close()
	// Close cleanly via CDP at the end; the defer kill is a safety net.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = browserClient.call(ctx, "Browser.close", map[string]any{})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	pageWS, err := waitPageTarget(port, 30*time.Second)
	if err != nil {
		return err
	}
	page, err := dial(pageWS)
	if err != nil {
		return fmt.Errorf("连接页面 CDP 失败: %w", err)
	}
	defer page.close()

	password := ""
	if opts.PasswordFile != "" {
		if b, err := os.ReadFile(opts.PasswordFile); err == nil {
			password = strings.TrimRight(string(b), "\r\n")
		}
		_ = os.Remove(opts.PasswordFile)
	}
	if opts.Username != "" && password != "" {
		// If the dedicated profile is already logged in, don't wait for a
		// login form; otherwise prefill it and let the user solve captcha.
		if _, _, err := readLogin(ctx, page, ""); err != nil {
			tryAutoLogin(ctx, page, opts.Username, password)
		}
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待登录超时（%s）", opts.Timeout)
		case <-ticker.C:
			name, cookie, err := readLogin(ctx, page, opts.MsName)
			if err != nil {
				continue
			}
			if cookie != "" {
				res.LoginName = name
				res.Cookie = cookie
				return nil
			}
		}
	}
}

func findBrowser() (string, error) {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("找不到系统 Edge/Chrome")
}

func waitDevToolsPort(profile string, timeout time.Duration) (int, string, error) {
	deadline := time.Now().Add(timeout)
	path := filepath.Join(profile, "DevToolsActivePort")
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
			if len(lines) >= 2 {
				var port int
				if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "%d", &port); err == nil && port > 0 {
					return port, strings.TrimSpace(lines[1]), nil
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return 0, "", errors.New("等待浏览器调试端口超时")
}

func waitPageTarget(port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		targets, err := listTargets(port)
		if err == nil {
			for _, t := range targets {
				if t.Type == "page" && strings.Contains(t.URL, "modelscope.cn") {
					return t.WebSocketDebuggerURL, nil
				}
			}
			for _, t := range targets {
				if t.Type == "page" && t.WebSocketDebuggerURL != "" {
					return t.WebSocketDebuggerURL, nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", errors.New("等待浏览器页面目标超时")
}

type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func listTargets(port int) ([]target, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out []target
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func tryAutoLogin(ctx context.Context, page *cdp, username, password string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := page.call(ctx, "Page.getFrameTree", map[string]any{})
		if err == nil {
			if frameID := findLoginFrame(raw); frameID != "" {
				wraw, werr := page.call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": "ms-login"})
				if werr == nil {
					var world struct {
						ExecutionContextID int `json:"executionContextId"`
					}
					if json.Unmarshal(wraw, &world) == nil && world.ExecutionContextID > 0 {
						userJSON, _ := json.Marshal(username)
						passJSON, _ := json.Marshal(password)
						expr := fmt.Sprintf(`(()=>{const set=(el,v)=>{if(!el)return;const d=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value');if(d&&d.set)d.set.call(el,v);else el.value=v;el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));};set(document.querySelector('#fm-login-id'),%s);set(document.querySelector('#fm-login-password'),%s);const cb=document.querySelector('#fm-agreement-checkbox');if(cb&&!cb.checked)cb.click();return true;})()`, string(userJSON), string(passJSON))
						_, _ = page.call(ctx, "Runtime.evaluate", map[string]any{"contextId": world.ExecutionContextID, "expression": expr, "returnByValue": true})
						time.Sleep(800 * time.Millisecond)
						click := `(()=>{const b=document.querySelector('.fm-submit');if(b&&!String(b.className).includes('disabled')){b.click();return true}return false})()`
						_, _ = page.call(ctx, "Runtime.evaluate", map[string]any{"contextId": world.ExecutionContextID, "expression": click, "returnByValue": true})
						return
					}
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func findLoginFrame(raw json.RawMessage) string {
	var doc struct {
		FrameTree struct {
			Frame struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"frame"`
			ChildFrames []struct {
				Frame struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"frame"`
				ChildFrames []any `json:"childFrames"`
			} `json:"childFrames"`
		} `json:"frameTree"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	for _, child := range doc.FrameTree.ChildFrames {
		if strings.Contains(child.Frame.URL, "mini_login") || strings.Contains(child.Frame.URL, "passport.modelscope.cn") {
			return child.Frame.ID
		}
	}
	return ""
}

func readLogin(ctx context.Context, page *cdp, wantName string) (string, string, error) {
	expr := `(async()=>{try{const r=await fetch('/api/v1/users/login/info',{credentials:'include',headers:{accept:'application/json'}});const j=await r.json();if(!j||!j.Data)return '';return JSON.stringify({name:j.Data.Name||j.Data.NickName||''});}catch(e){return ''}})()`
	raw, err := page.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"awaitPromise":  true,
		"returnByValue": true,
	})
	if err != nil {
		return "", "", err
	}
	var eval struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &eval) != nil || eval.Result.Value == "" {
		return "", "", errors.New("未登录")
	}
	var info struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(eval.Result.Value), &info) != nil || info.Name == "" {
		return "", "", errors.New("未登录")
	}
	if wantName != "" && info.Name != wantName {
		return "", "", fmt.Errorf("当前登录账号是 %s，不是 %s", info.Name, wantName)
	}
	cookies, err := getAllCookies(ctx, page)
	if err != nil {
		return "", "", err
	}
	return info.Name, cookies, nil
}

func getAllCookies(ctx context.Context, page *cdp) (string, error) {
	raw, err := page.call(ctx, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return "", err
	}
	var doc struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		} `json:"cookies"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", errors.New("解析 Cookie 失败")
	}
	parts := make([]string, 0, len(doc.Cookies))
	seen := map[string]bool{}
	for _, c := range doc.Cookies {
		if !strings.Contains(c.Domain, "modelscope.cn") {
			continue
		}
		if c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		parts = append(parts, c.Name+"="+c.Value)
	}
	if len(parts) == 0 {
		return "", errors.New("没有拿到 modelscope Cookie")
	}
	return strings.Join(parts, "; "), nil
}

type cdp struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	next    int
	pending map[int]chan cdpResponse
	done    chan struct{}
}

type cdpResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func dial(url string) (*cdp, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	c := &cdp{conn: conn, pending: map[int]chan cdpResponse{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *cdp) readLoop() {
	defer close(c.done)
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var r cdpResponse
		if json.Unmarshal(msg, &r) != nil || r.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch := c.pending[r.ID]
		delete(c.pending, r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

func (c *cdp) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan cdpResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	payload, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, fmt.Errorf("%s", r.Error.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("CDP 连接已关闭")
	}
}

func (c *cdp) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
