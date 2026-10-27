package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ttxyc/opencode-model-gateway/internal/app"
	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/platform"
)

var version = "0.1.0-dev"

func main() {
	var (
		service = flag.Bool("service", false, "run as Windows service")
		home    = flag.String("home", "", "data home directory")
		install = flag.Bool("install", false, "install Windows service (admin)")
		uninst  = flag.Bool("uninstall", false, "uninstall Windows service (admin)")
		start   = flag.Bool("start", false, "start Windows service")
		stop    = flag.Bool("stop", false, "stop Windows service")
		status  = flag.Bool("status", false, "show Windows service status")
		open    = flag.Bool("open-admin", false, "open the admin page")
		apply   = flag.Bool("apply-update", false, "internal update helper")
		newPath = flag.String("new", "", "new executable for --apply-update")
		showVer = flag.Bool("version", false, "print version")
		run     = flag.Bool("run", false, "run in console mode")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}
	h := *home
	if h == "" {
		h = config.DefaultHome()
	}
	switch {
	case *service:
		if err := platform.RunService(h, version); err != nil {
			os.Exit(1)
		}
	case *install:
		if err := platform.Install(h, version); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("service installed:", config.ServiceName)
	case *uninst:
		if err := platform.Uninstall(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("service removed")
	case *start:
		if err := platform.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("service started")
	case *stop:
		if err := platform.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("service stopped")
	case *status:
		st, err := platform.Status()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(st)
	case *open:
		if err := platform.OpenAdmin(h); err != nil {
			platform.ShowError("无法打开网关管理页面: " + err.Error())
			os.Exit(1)
		}
	case *apply:
		if *newPath == "" {
			fmt.Fprintln(os.Stderr, "--new is required")
			os.Exit(1)
		}
		if err := platform.ApplyUpdate(h, *newPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *run:
		runConsole(h)
	default:
		fmt.Fprintf(os.Stderr, "opencode-model-gateway %s\nUse --run for console, --install/--uninstall/--start/--stop/--status for service, --open-admin for UI, --version.\n", version)
		os.Exit(2)
	}
}

func runConsole(home string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, home, app.Options{Version: version, ApplyFunc: func(string) error {
		return fmt.Errorf("update apply is not available in console mode")
	}}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
