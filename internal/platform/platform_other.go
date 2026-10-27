//go:build !windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ttxyc/opencode-model-gateway/internal/app"
)

func IsElevated() bool { return false }

func RunService(home, version string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return app.Run(ctx, home, app.Options{Version: version, ApplyFunc: func(string) error {
		return errors.New("apply-update is only supported on Windows")
	}})
}

func Install(home, version string) error {
	return errors.New("service installation is only supported on Windows")
}
func DefaultAction(home string) error {
	fmt.Println("double-click installer is only supported on Windows")
	return nil
}
func InstallEmbedded(home string) error {
	return errors.New("embedded installer is only supported on Windows")
}
func ElevateInstall(home string) error {
	return errors.New("embedded installer is only supported on Windows")
}
func Uninstall() error        { return errors.New("only supported on Windows") }
func Start() error            { return errors.New("only supported on Windows") }
func Stop() error             { return errors.New("only supported on Windows") }
func Status() (string, error) { return "", errors.New("only supported on Windows") }
func OpenAdmin(home string) error {
	fmt.Println("service runs in console mode on this platform")
	return nil
}
func PrepareApply(home, newPath string) error { return errors.New("only supported on Windows") }
func ApplyUpdate(home, newPath string) error  { return errors.New("only supported on Windows") }
func ShowError(text string)                   { fmt.Fprintln(os.Stderr, text) }
