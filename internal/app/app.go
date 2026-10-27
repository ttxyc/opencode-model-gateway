package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ttxyc/opencode-model-gateway/internal/api"
	"github.com/ttxyc/opencode-model-gateway/internal/config"
	"github.com/ttxyc/opencode-model-gateway/internal/logx"
	"github.com/ttxyc/opencode-model-gateway/internal/router"
	"github.com/ttxyc/opencode-model-gateway/internal/updater"
)

type Options struct {
	Version   string
	ApplyFunc func(path string) error
}

func Run(ctx context.Context, home string, opts Options) error {
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	log, err := logx.New(cfg.Log.Dir, cfg.Log.MaxBytes, cfg.Log.MaxFiles)
	if err != nil {
		return err
	}
	log.Info("startup", map[string]any{
		"version": opts.Version, "home": cfg.Home, "listen": fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port),
		"providers": len(cfg.Providers),
	})
	r := router.New(cfg, log)
	initCtx, cancelInit := context.WithTimeout(ctx, 30*time.Second)
	go func() {
		r.RefreshAll(initCtx)
		cancelInit()
		log.Info("router_ready", map[string]any{"providers": r.Status()})
	}()

	var update *updater.Manager
	if cfg.Update.Enabled {
		update = updater.New(cfg, opts.Version, log, opts.ApplyFunc)
	}
	server := api.New(cfg, r, log, api.Options{
		Version: opts.Version, StartedAt: time.Now(), Update: update, ApplyUpdate: opts.ApplyFunc,
		AdminToken: cfg.AdminToken, ClientToken: cfg.ClientToken,
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port))
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Info("listening", map[string]any{"addr": ln.Addr().String()})

	balance := time.NewTicker(60 * time.Second)
	defer balance.Stop()
	watchdog := time.NewTicker(15 * time.Second)
	defer watchdog.Stop()
	updateTicker := time.NewTicker(time.Duration(cfg.Update.CheckHours) * time.Hour)
	defer updateTicker.Stop()

	var failures int
	go func() {
		if update == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		checkUpdate(ctx, update, log, cfg.Update.AllowRestart)
	}()

	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = httpServer.Shutdown(shutdown)
			cancel()
			log.Info("shutdown", map[string]any{})
			return nil
		case err := <-errCh:
			return err
		case <-balance.C:
			go r.RefreshAll(context.Background())
		case <-watchdog.C:
			if os.Getenv("GATEWAY_DISABLE_WATCHDOG") == "1" {
				continue
			}
			url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)
			reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp != nil {
				_ = resp.Body.Close()
			}
			cancel()
			if err != nil {
				failures++
				log.Warn("watchdog_fail", map[string]any{"failures": failures, "error": logx.Redact(err.Error(), 300)})
				if failures >= 3 {
					log.Crash("watchdog_exit", map[string]any{"message": "health check failed 3 times"})
					_ = httpServer.Close()
					return errors.New("watchdog: health check failed")
				}
			} else {
				failures = 0
			}
		case <-updateTicker.C:
			if update != nil {
				go checkUpdate(ctx, update, log, cfg.Update.AllowRestart)
			}
		}
	}
}

func checkUpdate(ctx context.Context, u *updater.Manager, log *logx.Logger, allowRestart bool) {
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	st, err := u.Check(checkCtx)
	if err != nil {
		log.Warn("update_check_failed", map[string]any{"error": logx.Redact(err.Error(), 300)})
		return
	}
	if !st.Available {
		return
	}
	log.Info("update_available", map[string]any{"current": st.Current, "latest": st.Latest})
	if allowRestart {
		if _, err := u.Apply(checkCtx); err != nil {
			log.Error("update_apply_failed", map[string]any{"error": logx.Redact(err.Error(), 500)})
		}
	}
}
