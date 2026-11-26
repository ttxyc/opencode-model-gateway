//go:build windows

package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/ttxyc/opencode-model-gateway/internal/modelscope"
)

const (
	toolsDir = `C:\ProgramData\opencode-model-gateway\scripts\tools`

	wbClaimDir   = `D:\wsl-opencode\workbuddy-claim`
	wbStatusFile = wbClaimDir + `\.gateway-status.json`
	wbClaimFile  = wbClaimDir + `\.gateway-claim.json`
	wbTokenFile  = wbClaimDir + `\.gateway-token.json`

	msDir  = `D:\wsl-opencode\modelscope-auto`
	msLogs = msDir + `\logs`

	wbStatusTask  = "Tools-WorkBuddy-Status"
	wbClaimTask   = "Tools-WorkBuddy-Claim"
	wbTokenTask   = "Tools-WorkBuddy-RefreshToken"
	wbPanelTask   = "Tools-WorkBuddy-Panel"
	msRunTask     = "Tools-ModelScope-Run"
	msRefreshTask = "Tools-ModelScope-Refresh"
	msLoginTask   = "Tools-ModelScope-Login"
	msAutoTask    = "ModelScope-Daily-Magicube"
)

type toolUser struct {
	User string `json:"user"`
	SID  string `json:"sid"`
	Name string `json:"name"`
	Err  string `json:"error"`
}

type msAccount struct {
	Name    string `json:"name"`
	MsName  string `json:"msName,omitempty"`
	Enabled bool   `json:"enabled"`
	Done    bool   `json:"done"`
}

type msState struct {
	Date     string   `json:"date"`
	DoneAt   string   `json:"doneAt"`
	Accounts []string `json:"accounts"`
}

type msTask struct {
	Exists     bool   `json:"exists"`
	State      string `json:"state,omitempty"`
	LastRun    string `json:"lastRun,omitempty"`
	LastResult int    `json:"lastResult,omitempty"`
	NextRun    string `json:"nextRun,omitempty"`
}

type msProgress struct {
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Current   string `json:"current,omitempty"`
	Phase     string `json:"phase,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type msAction struct {
	Action     string      `json:"action"`
	Running    bool        `json:"running"`
	LastExit   int         `json:"lastExit"`
	StartedAt  string      `json:"startedAt,omitempty"`
	FinishedAt string      `json:"finishedAt,omitempty"`
	Progress   *msProgress `json:"progress,omitempty"`
}

type msStatus struct {
	Dir          string      `json:"dir"`
	Today        string      `json:"today"`
	Accounts     []msAccount `json:"accounts"`
	EnabledCount int         `json:"enabledCount"`
	DoneToday    bool        `json:"doneToday"`
	DoneAt       string      `json:"doneAt,omitempty"`
	LogFile      string      `json:"logFile,omitempty"`
	LogTail      string      `json:"logTail"`
	AutoTask     msTask      `json:"autoTask"`
	RunTask      msTask      `json:"runTask"`
	RefreshTask  msTask      `json:"refreshTask"`
	Run          msAction    `json:"run"`
	Refresh      msAction    `json:"refresh"`
	Error        string      `json:"error,omitempty"`
}

func powershellExe() string {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func hideWindow(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func runPowerShell(script string) ([]byte, error) {
	cmd := hideWindow(exec.Command(powershellExe(), "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script))
	return cmd.CombinedOutput()
}

func interactiveUser() (toolUser, error) {
	script := `
$u = ''
try { $p = Get-Process explorer -IncludeUserName -ErrorAction SilentlyContinue | Select-Object -First 1; if ($p) { $u = $p.UserName } } catch {}
if (-not $u) { try { $u = (Get-CimInstance Win32_ComputerSystem -ErrorAction Stop).UserName } catch {} }
if (-not $u) { '{"error":"no interactive user"}'; exit 0 }
try {
  $sid = (New-Object Security.Principal.NTAccount($u)).Translate([Security.Principal.SecurityIdentifier]).Value
} catch { '{"error":"cannot resolve user SID"}'; exit 0 }
$name = ($u -split '\\')[-1]
[pscustomobject]@{user=$u;sid=$sid;name=$name} | ConvertTo-Json -Compress
`
	out, err := runPowerShell(script)
	if err != nil {
		return toolUser{}, fmt.Errorf("detect interactive user: %w: %s", err, strings.TrimSpace(string(out)))
	}
	raw := extractJSONObject(string(out))
	var u toolUser
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return toolUser{}, fmt.Errorf("detect interactive user: invalid output %q", strings.TrimSpace(string(out)))
	}
	if u.Err != "" || u.SID == "" || u.Name == "" {
		msg := u.Err
		if msg == "" {
			msg = "没有检测到已登录的 Windows 用户"
		}
		return toolUser{}, errors.New(msg)
	}
	return u, nil
}

func extractJSONObject(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return strings.TrimSpace(s)
}

func ensureToolsFiles() error {
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"wb-status.cmd": `@echo off
setlocal
set "OUT=D:\wsl-opencode\workbuddy-claim\.gateway-status.json"
del "%OUT%" "%OUT%.err" "%OUT%.done" "%OUT%.start" >nul 2>&1
echo start > "%OUT%.start"
C:\Windows\System32\wsl.exe -d Ubuntu -- bash -lc "source $HOME/.nvm/nvm.sh >/dev/null 2>&1; cd /mnt/d/wsl-opencode/workbuddy-claim && node status.js --json" > "%OUT%" 2> "%OUT%.err"
echo %ERRORLEVEL% > "%OUT%.done"
`,
		"wb-claim.cmd": `@echo off
setlocal
set "OUT=D:\wsl-opencode\workbuddy-claim\.gateway-claim.json"
del "%OUT%" "%OUT%.err" "%OUT%.done" "%OUT%.start" >nul 2>&1
echo start > "%OUT%.start"
C:\Windows\System32\wsl.exe -d Ubuntu -- bash -lc "source $HOME/.nvm/nvm.sh >/dev/null 2>&1; cd /mnt/d/wsl-opencode/workbuddy-claim && node claim.js --apply --json" > "%OUT%" 2> "%OUT%.err"
echo %ERRORLEVEL% > "%OUT%.done"
`,
		"wb-refresh-token.cmd": `@echo off
setlocal
set "OUT=D:\wsl-opencode\workbuddy-claim\.gateway-token.json"
del "%OUT%" "%OUT%.err" "%OUT%.done" "%OUT%.start" >nul 2>&1
echo start > "%OUT%.start"
C:\Windows\System32\wsl.exe -d Ubuntu -- bash -lc "source $HOME/.nvm/nvm.sh >/dev/null 2>&1; cd /mnt/d/wsl-opencode/workbuddy-claim && node refresh-token.js --json" > "%OUT%" 2> "%OUT%.err"
echo %ERRORLEVEL% > "%OUT%.done"
`,
		"ms-run.cmd": `@echo off
setlocal
set "DIR=D:\wsl-opencode\modelscope-auto"
set "LOG=%DIR%\logs\gateway-run.log"
set "START=%DIR%\logs\.gateway-run.start"
set "DONE=%DIR%\logs\.gateway-run.done"
del "%START%" "%DONE%" >nul 2>&1
echo start > "%START%"
C:\Windows\System32\wsl.exe -d Ubuntu -- bash -lc "source $HOME/.nvm/nvm.sh >/dev/null 2>&1; cd /mnt/d/wsl-opencode/modelscope-auto && mkdir -p logs && node check.js --force" >> "%LOG%" 2>&1
set "RC=%ERRORLEVEL%"
echo %RC% > "%DONE%"
`,
		"ms-refresh.cmd": `@echo off
setlocal
set "DIR=D:\wsl-opencode\modelscope-auto"
set "LOG=%DIR%\logs\gateway-refresh.log"
set "START=%DIR%\logs\.gateway-refresh.start"
set "DONE=%DIR%\logs\.gateway-refresh.done"
del "%START%" "%DONE%" >nul 2>&1
echo start > "%START%"
C:\Windows\System32\wsl.exe -d Ubuntu -- bash -lc "source $HOME/.nvm/nvm.sh >/dev/null 2>&1; cd /mnt/d/wsl-opencode/modelscope-auto && node refresh-cookies.js" >> "%LOG%" 2>&1
set "RC=%ERRORLEVEL%"
echo %RC% > "%DONE%"
`,
		"wb-status.vbs": `Set sh = CreateObject("WScript.Shell")
sh.Run "cmd.exe /c ""C:\ProgramData\opencode-model-gateway\scripts\tools\wb-status.cmd""", 0, True
`,
		"wb-claim.vbs": `Set sh = CreateObject("WScript.Shell")
sh.Run "cmd.exe /c ""C:\ProgramData\opencode-model-gateway\scripts\tools\wb-claim.cmd""", 0, True
`,
		"wb-refresh-token.vbs": `Set sh = CreateObject("WScript.Shell")
sh.Run "cmd.exe /c ""C:\ProgramData\opencode-model-gateway\scripts\tools\wb-refresh-token.cmd""", 0, True
`,
		"ms-run.vbs": `Set sh = CreateObject("WScript.Shell")
sh.Run "cmd.exe /c ""C:\ProgramData\opencode-model-gateway\scripts\tools\ms-run.cmd""", 0, True
`,
		"ms-refresh.vbs": `Set sh = CreateObject("WScript.Shell")
sh.Run "cmd.exe /c ""C:\ProgramData\opencode-model-gateway\scripts\tools\ms-refresh.cmd""", 0, True
`,
	}
	for name, content := range files {
		content = strings.ReplaceAll(content, "\n", "\r\n")
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	// Let the interactive user read/execute the wrappers. Ignore failures:
	// ProgramData's default ACL already grants Users read on new files.
	cmd := hideWindow(exec.Command("icacls.exe", toolsDir, "/grant", "BUILTIN\\Users:(OI)(CI)(RX)", "/T", "/C", "/Q"))
	_, _ = cmd.CombinedOutput()
	return nil
}

func taskExists(name string) bool {
	cmd := hideWindow(exec.Command("schtasks.exe", "/Query", "/TN", name))
	return cmd.Run() == nil
}

func registerTask(name, command, arguments, sid string) error {
	// Always recreate with /F: the ModelScope login helper passes a new result
	// path/account on every run, so reusing an old task definition would run
	// stale arguments.
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.3" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Author>opencode-model-gateway</Author><URI>\%s</URI></RegistrationInfo>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <StartWhenAvailable>true</StartWhenAvailable>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
  </Settings>
  <Triggers><CalendarTrigger><StartBoundary>2037-01-01T00:00:00</StartBoundary><Enabled>false</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger></Triggers>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions>
</Task>
`, html.EscapeString(name), html.EscapeString(sid), html.EscapeString(command), html.EscapeString(arguments))
	tmp, err := os.CreateTemp("", "gateway-tools-task-*.xml")
	if err != nil {
		return err
	}
	path := tmp.Name()
	_, _ = tmp.Write(utf16LEWithBOM(xml))
	_ = tmp.Close()
	defer os.Remove(path)
	out, err := hideWindow(exec.Command("schtasks.exe", "/Create", "/TN", name, "/XML", path, "/F")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create task %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func utf16LEWithBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2+i*2:], c)
	}
	return b
}

func triggerTask(name string) error {
	out, err := hideWindow(exec.Command("schtasks.exe", "/Run", "/TN", name)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("start task %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitForDone(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() >= 0 {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("等待任务完成超时（%s）", path)
}

func readResult(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		errText := ""
		if b, err := os.ReadFile(path + ".err"); err == nil {
			errText = strings.TrimSpace(string(b))
			if len(errText) > 1000 {
				errText = errText[len(errText)-1000:]
			}
		}
		if errText == "" {
			errText = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("工具返回不是有效 JSON: %s", errText)
	}
	return raw, nil
}

func runUserTaskSync(name, command, arguments, resultPath string, sid string) ([]byte, error) {
	if err := registerTask(name, command, arguments, sid); err != nil {
		return nil, err
	}
	_ = os.Remove(resultPath)
	_ = os.Remove(resultPath + ".done")
	_ = os.Remove(resultPath + ".start")
	if err := triggerTask(name); err != nil {
		return nil, err
	}
	if err := waitForDone(resultPath+".done", toolTimeout); err != nil {
		return nil, err
	}
	return readResult(resultPath)
}

func workBuddyToolStatus() ([]byte, error) {
	if _, err := os.Stat(wbClaimDir); err != nil {
		return nil, fmt.Errorf("找不到 WorkBuddy Claim 项目: %s", wbClaimDir)
	}
	u, err := interactiveUser()
	if err != nil {
		return nil, err
	}
	return runUserTaskSync(wbStatusTask, `C:\Windows\System32\wscript.exe`, `"`+toolsDir+`\wb-status.vbs"`, wbStatusFile, u.SID)
}

func workBuddyToolClaim() ([]byte, error) {
	if _, err := os.Stat(wbClaimDir); err != nil {
		return nil, fmt.Errorf("找不到 WorkBuddy Claim 项目: %s", wbClaimDir)
	}
	u, err := interactiveUser()
	if err != nil {
		return nil, err
	}
	return runUserTaskSync(wbClaimTask, `C:\Windows\System32\wscript.exe`, `"`+toolsDir+`\wb-claim.vbs"`, wbClaimFile, u.SID)
}

func workBuddyToolRefreshToken() ([]byte, error) {
	if _, err := os.Stat(wbClaimDir); err != nil {
		return nil, fmt.Errorf("找不到 WorkBuddy Claim 项目: %s", wbClaimDir)
	}
	if _, err := os.Stat(filepath.Join(wbClaimDir, "refresh-token.js")); err != nil {
		return nil, fmt.Errorf("找不到 refresh-token.js，请先同步 workbuddy-claim 项目")
	}
	u, err := interactiveUser()
	if err != nil {
		return nil, err
	}
	return runUserTaskSync(wbTokenTask, `C:\Windows\System32\wscript.exe`, `"`+toolsDir+`\wb-refresh-token.vbs"`, wbTokenFile, u.SID)
}

func (s *Server) setupModelScopeLogin() {
	if s.ms == nil {
		return
	}
	s.ms.SetLoginFunc(func(ctx context.Context, acc modelscope.Account) (string, error) {
		return s.modelScopeLogin(ctx, acc)
	})
}

func safeProfileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}

func quoteTaskArg(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \t\"") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

func (s *Server) modelScopeLogin(ctx context.Context, acc modelscope.Account) (string, error) {
	u, err := interactiveUser()
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resultPath := filepath.Join(`C:\Users`, u.Name, `AppData\Local\Temp`, fmt.Sprintf("opencode-ms-login-%d.json", time.Now().UnixNano()))
	profileName := acc.MsName
	if profileName == "" {
		profileName = acc.Name
	}
	profile := filepath.Join(`C:\Users`, u.Name, `AppData\Local\opencode-model-gateway\login-profile`, "modelscope-"+safeProfileName(profileName))
	passPath := filepath.Join(`C:\Users`, u.Name, `AppData\Local\Temp`, fmt.Sprintf("opencode-ms-pass-%d.txt", time.Now().UnixNano()))
	_ = os.Remove(resultPath)
	if acc.Password != "" {
		if err := os.WriteFile(passPath, []byte(acc.Password), 0o600); err != nil {
			return "", fmt.Errorf("写入临时密码文件失败: %w", err)
		}
		defer os.Remove(passPath)
	}
	username := acc.MsName
	if username == "" {
		username = acc.Name
	}
	argsList := []string{
		"--login-helper",
		"--provider=modelscope",
		"--account=" + quoteTaskArg(acc.Name),
		"--ms-name=" + quoteTaskArg(acc.MsName),
		"--username=" + quoteTaskArg(username),
		"--login-url=" + quoteTaskArg("https://www.modelscope.cn/login"),
		"--result=" + quoteTaskArg(resultPath),
		"--profile=" + quoteTaskArg(profile),
	}
	if acc.Password != "" {
		argsList = append(argsList, "--password-file="+quoteTaskArg(passPath))
	}
	args := strings.Join(argsList, " ")
	if err := registerTask(msLoginTask, exe, args, u.SID); err != nil {
		return "", err
	}
	if err := triggerTask(msLoginTask); err != nil {
		return "", err
	}
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if b, err := os.ReadFile(resultPath); err == nil {
			var res struct {
				OK        bool   `json:"ok"`
				LoginName string `json:"loginName"`
				Cookie    string `json:"cookie"`
				Error     string `json:"error"`
			}
			if json.Unmarshal(b, &res) == nil {
				_ = os.Remove(resultPath)
				if !res.OK {
					if res.Error == "" {
						res.Error = "登录失败"
					}
					return "", errors.New(res.Error)
				}
				if res.Cookie == "" {
					return "", errors.New("登录助手没有返回 Cookie")
				}
				return res.Cookie, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return "", errors.New("等待浏览器登录超时")
}

func workBuddyToolPanel() error {
	u, err := interactiveUser()
	if err != nil {
		return err
	}
	exe := filepath.Join(`C:\Users`, u.Name, `AppData\Local\WorkBuddyClaim\WorkBuddyClaim.exe`)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("找不到 WorkBuddyClaim.exe（%s），请先双击桌面的 WorkBuddy Claim", exe)
	}
	if err := registerTask(wbPanelTask, exe, "", u.SID); err != nil {
		return err
	}
	return triggerTask(wbPanelTask)
}

// queryTasks reads several scheduled tasks in one PowerShell call. Calling
// queryTask per task tripled the status latency (~9s), which made the run
// progress bar very sluggish.
func queryTasks(names []string) map[string]msTask {
	out := make(map[string]msTask, len(names))
	if len(names) == 0 {
		return out
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	script := fmt.Sprintf(`
$names = @(%s)
$o = [ordered]@{}
foreach ($n in $names) {
  $t = Get-ScheduledTask -TaskName $n -ErrorAction SilentlyContinue
  if (-not $t) { $o[$n] = [ordered]@{exists=$false}; continue }
  $item = [ordered]@{exists=$true;state=[string]$t.State;lastRun="";lastResult=0;nextRun=""}
  try { $i = Get-ScheduledTaskInfo -TaskName $n -ErrorAction Stop; $item.lastRun=[string]$i.LastRunTime; $item.lastResult=$i.LastTaskResult; $item.nextRun=[string]$i.NextRunTime } catch {}
  $o[$n] = $item
}
$o | ConvertTo-Json -Compress -Depth 5
`, strings.Join(quoted, ","))
	raw, err := runPowerShell(script)
	if err != nil {
		return out
	}
	var m map[string]msTask
	if json.Unmarshal([]byte(extractJSONObject(string(raw))), &m) != nil {
		return out
	}
	return m
}

var msTaskCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string]msTask
}

// queryTasksCached keeps the admin status endpoint fast while it is polled
// every few seconds during a run. Scheduled-task state changes slowly; the
// live run progress comes from the marker/progress files, not this cache.
func queryTasksCached(names []string) map[string]msTask {
	msTaskCache.mu.Lock()
	defer msTaskCache.mu.Unlock()
	if msTaskCache.data != nil && time.Since(msTaskCache.at) < 20*time.Second {
		return msTaskCache.data
	}
	data := queryTasks(names)
	if len(data) > 0 {
		msTaskCache.data = data
		msTaskCache.at = time.Now()
	}
	return data
}

func readMSAction(action string) msAction {
	start := filepath.Join(msLogs, ".gateway-"+action+".start")
	done := filepath.Join(msLogs, ".gateway-"+action+".done")
	out := msAction{Action: action}
	startInfo, startErr := os.Stat(start)
	doneInfo, doneErr := os.Stat(done)
	if startErr == nil {
		out.StartedAt = startInfo.ModTime().Format("2006-01-02 15:04:05")
		out.Running = doneErr != nil || doneInfo.ModTime().Before(startInfo.ModTime())
	}
	if doneErr == nil {
		out.FinishedAt = doneInfo.ModTime().Format("2006-01-02 15:04:05")
		if b, err := os.ReadFile(done); err == nil {
			fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &out.LastExit)
		}
	}
	if out.Running && startErr == nil {
		pf := filepath.Join(msLogs, ".gateway-"+action+".progress.json")
		if fi, err := os.Stat(pf); err == nil && !fi.ModTime().Before(startInfo.ModTime()) {
			if b, err := os.ReadFile(pf); err == nil {
				var p msProgress
				if json.Unmarshal(b, &p) == nil {
					out.Progress = &p
				}
			}
		}
	}
	return out
}

func latestMSLog() (string, string) {
	entries, err := os.ReadDir(msLogs)
	if err != nil {
		return "", ""
	}
	var newest os.DirEntry
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(newestTime) {
			newest, newestTime = e, info.ModTime()
		}
	}
	if newest == nil {
		return "", ""
	}
	path := filepath.Join(msLogs, newest.Name())
	raw, err := os.ReadFile(path)
	if err != nil {
		return path, ""
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > 120 {
		lines = lines[len(lines)-120:]
	}
	return path, strings.Join(lines, "\n")
}

func modelScopeToolStatus() (msStatus, error) {
	out := msStatus{Dir: msDir, Today: time.Now().Format("2006-01-02")}
	raw, err := os.ReadFile(filepath.Join(msDir, "accounts.json"))
	if err != nil {
		return out, fmt.Errorf("读取 modelscope accounts.json 失败: %w", err)
	}
	var accFile struct {
		Accounts []struct {
			Name    string `json:"name"`
			MsName  string `json:"msName"`
			Enabled *bool  `json:"enabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &accFile); err != nil {
		return out, fmt.Errorf("解析 modelscope accounts.json 失败: %w", err)
	}
	var st msState
	if b, err := os.ReadFile(filepath.Join(msDir, ".state.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	doneSet := map[string]bool{}
	if st.Date == out.Today {
		out.DoneToday = true
		out.DoneAt = st.DoneAt
		for _, n := range st.Accounts {
			doneSet[n] = true
		}
	}
	for _, a := range accFile.Accounts {
		enabled := true
		if a.Enabled != nil {
			enabled = *a.Enabled
		}
		if enabled {
			out.EnabledCount++
		}
		out.Accounts = append(out.Accounts, msAccount{Name: a.Name, MsName: a.MsName, Enabled: enabled, Done: doneSet[a.Name]})
	}
	out.LogFile, out.LogTail = latestMSLog()
	tasks := queryTasksCached([]string{msAutoTask, msRunTask, msRefreshTask})
	out.AutoTask = tasks[msAutoTask]
	out.RunTask = tasks[msRunTask]
	out.RefreshTask = tasks[msRefreshTask]
	out.Run = readMSAction("run")
	out.Refresh = readMSAction("refresh")
	return out, nil
}

func modelScopeToolAction(action string) (map[string]any, error) {
	u, err := interactiveUser()
	if err != nil {
		return nil, err
	}
	var task, script, label string
	switch action {
	case "run":
		task, script, label = msRunTask, `ms-run.vbs`, "立即运行"
	case "refresh":
		task, script, label = msRefreshTask, `ms-refresh.vbs`, "刷新 Cookie"
	default:
		return nil, errors.New("未知 ModelScope 动作")
	}
	if m := readMSAction(action); m.Running {
		return nil, errors.New("ModelScope 任务正在运行中，请稍候")
	}
	if err := registerTask(task, `C:\Windows\System32\wscript.exe`, `"`+toolsDir+`\`+script+`"`, u.SID); err != nil {
		return nil, err
	}
	_ = os.Remove(filepath.Join(msLogs, ".gateway-"+action+".done"))
	_ = os.Remove(filepath.Join(msLogs, ".gateway-"+action+".start"))
	_ = os.Remove(filepath.Join(msLogs, ".gateway-"+action+".progress.json"))
	if err := triggerTask(task); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "action": action, "message": label + " 已触发",
		"startedAt": time.Now().Format("2006-01-02 15:04:05"),
	}, nil
}
