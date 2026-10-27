//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/app"
	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = config.ServiceName
	displayName = "opencode Model Gateway"
)

func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

type handler struct {
	home    string
	version string
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Run(ctx, h.home, app.Options{Version: h.version, ApplyFunc: func(path string) error {
			return PrepareApply(h.home, path)
		}})
	}()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-errCh:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}

func RunService(home, version string) error {
	return svc.Run(serviceName, &handler{home: home, version: version})
}

func Install(home, version string) error {
	if !IsElevated() {
		return errors.New("service installation requires administrator privileges")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		_ = s.Delete()
		_ = s.Close()
		time.Sleep(500 * time.Millisecond)
	}
	cfg := mgr.Config{
		DisplayName:      displayName,
		Description:      "Local OpenAI-compatible gateway for ModelScope/Cline/Atria/AMD/DeepSeek.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}
	s, err := m.CreateService(serviceName, exe, cfg, "--service", "--home", home)
	if err != nil {
		return err
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400)
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	return s.Start()
}

func Uninstall() error {
	if !IsElevated() {
		return errors.New("service uninstallation requires administrator privileges")
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	return s.Delete()
}

func Start() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

func Stop() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = s.Control(svc.Stop)
	return err
}

func Status() (string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return "", err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return "", err
	}
	switch st.State {
	case svc.Stopped:
		return "Stopped", nil
	case svc.StartPending:
		return "StartPending", nil
	case svc.StopPending:
		return "StopPending", nil
	case svc.Running:
		return "Running", nil
	case svc.Paused:
		return "Paused", nil
	default:
		return fmt.Sprintf("%d", st.State), nil
	}
}

func OpenAdmin(home string) error {
	url := "http://127.0.0.1:8788/admin"
	if cfg, err := config.Load(home); err == nil {
		url = fmt.Sprintf("http://127.0.0.1:%d/admin", cfg.Port)
	}
	if m, err := mgr.Connect(); err == nil {
		if s, err := m.OpenService(serviceName); err == nil {
			st, _ := s.Query()
			if st.State == svc.Stopped || st.State == svc.StopPending {
				_ = s.Start()
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if st, _ := s.Query(); st.State == svc.Running {
						break
					}
					time.Sleep(250 * time.Millisecond)
				}
			}
			_ = s.Close()
		}
		_ = m.Disconnect()
	}
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
}

func PrepareApply(home, newPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--apply-update", "--home", home, "--new", newPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}

func ApplyUpdate(home, newPath string) error {
	log, _ := logx.New(filepath.Join(home, "logs"), 5*1024*1024, 3)
	logf := func(msg string, fields map[string]any) {
		if log != nil {
			log.Info(msg, fields)
		}
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	conf, err := s.Config()
	if err != nil {
		return err
	}
	exe := serviceExe(conf.BinaryPathName)
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	logf("update_start", map[string]any{"exe": exe, "new": newPath})
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := s.Query(); st.State == svc.Stopped {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	backup := exe + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		logf("update_rename_failed", map[string]any{"error": err.Error()})
		_ = s.Start()
		return err
	}
	if err := copyFile(newPath, exe); err != nil {
		_ = os.Rename(backup, exe)
		_ = s.Start()
		logf("update_copy_failed", map[string]any{"error": err.Error()})
		return err
	}
	if err := s.Start(); err != nil {
		_ = os.Remove(exe)
		_ = os.Rename(backup, exe)
		_ = s.Start()
		logf("update_start_failed", map[string]any{"error": err.Error()})
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)
	ok := false
	hc := &http.Client{Timeout: 3 * time.Second}
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := hc.Get(url)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ok = true
				break
			}
		}
	}
	if !ok {
		_, _ = s.Control(svc.Stop)
		time.Sleep(2 * time.Second)
		_ = os.Remove(exe)
		_ = os.Rename(backup, exe)
		_ = s.Start()
		logf("update_health_failed", map[string]any{})
		return errors.New("new version failed health check; rolled back")
	}
	logf("update_ok", map[string]any{})
	return nil
}

func serviceExe(binPath string) string {
	binPath = strings.TrimSpace(binPath)
	if strings.HasPrefix(binPath, "\"") {
		if i := strings.Index(binPath[1:], "\""); i >= 0 {
			return binPath[1 : i+1]
		}
	}
	if i := strings.IndexAny(binPath, " \t"); i > 0 {
		return binPath[:i]
	}
	return binPath
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func ShowError(text string) {
	p, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("opencode Model Gateway")
	_, _ = windows.MessageBox(0, p, c, windows.MB_OK|windows.MB_ICONERROR)
}
