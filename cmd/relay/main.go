package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mini-cage-relay/internal/accounts"
	"mini-cage-relay/internal/api"
	"mini-cage-relay/internal/claude"
	"mini-cage-relay/internal/exitpolicy"
	"mini-cage-relay/internal/mihomo"
	"mini-cage-relay/internal/safety"
	"mini-cage-relay/internal/updater"
)

func main() {
	listenAddr := envOr("RELAY_LISTEN_ADDR", ":8317")
	proxyURL := envOr("RELAY_PROXY_URL", "http://gateway:7890")
	gwControlURL := envOr("RELAY_GATEWAY_CONTROL_URL", "http://gateway:18080")
	internalToken := strings.TrimSpace(os.Getenv("INTERNAL_TOKEN"))
	apiKey := strings.TrimSpace(os.Getenv("RELAY_API_KEY"))
	accountsDir := envOr("ACCOUNTS_DIR", "/data/accounts")
	policyFile := envOr("POLICY_FILE", "/data/config/exit_policy.json")
	envSyncFile := envOr("ENV_SYNC_FILE", "/data/runtime/env_sync.json")
	claudeBin := envOr("CLAUDE_BIN", "claude")
	defaultModel := envOr("DEFAULT_MODEL", "sonnet")
	devMode := strings.EqualFold(os.Getenv("ALLOW_HOST_DEV_MODE"), "true")

	var extraArgs []string
	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CLI_EXTRA_ARGS")); raw != "" {
		extraArgs = strings.Fields(raw)
	}

	pool, err := accounts.NewPool(accountsDir)
	if err != nil {
		log.Fatalf("init accounts pool: %v", err)
	}

	pol := loadOrDefaultPolicy(policyFile)

	var srv *api.Server
	checker := safety.NewChecker(safety.CheckerConfig{
		ProxyURL:          proxyURL,
		AllowHostDevMode:  devMode,
		AccountsDir:       accountsDir,
		EnvSyncOutputFile: envSyncFile,
		FetchGateway: func(ctx context.Context) (*mihomo.Status, error) {
			return srv.FetchGatewayStatus(ctx)
		},
	}, pol)

	runner := claude.NewRunner(claudeBin, defaultModel, extraArgs)
	srv = api.NewServer(api.ServerConfig{
		APIKey:            apiKey,
		ProxyURL:          proxyURL,
		GatewayControlURL: gwControlURL,
		InternalToken:     internalToken,
		PolicyFile:        policyFile,
	}, checker, pool, runner)

	upd := updater.NewChecker(updater.Config{
		ProxyURL:           proxyURL,
		ClaudeBin:          claudeBin,
		DesktopVersionFile: "/data/runtime/desktop_versions.json",
		GetMihomoVersion: func(ctx context.Context) string {
			if st, err := srv.FetchGatewayStatus(ctx); err == nil && st != nil {
				return st.MihomoVersion
			}
			return ""
		},
	})
	srv.SetUpdater(upd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Auto-check component updates shortly after startup and every 6 hours through the isolated proxy.
	go func() {
		time.Sleep(8 * time.Second)
		for {
			rep := upd.CheckAll(ctx)
			log.Printf("[relay] update check done has_updates=%v count=%d", rep.HasUpdates, rep.UpdateCount)
			select {
			case <-ctx.Done():
				return
			case <-time.After(6 * time.Hour):
			}
		}
	}()

	// Run initial safety check shortly after boot; auto-recover every 20s when tripped, 10m when healthy.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		for {
			rep := checker.RunAll(ctx)
			log.Printf("[relay] safety check authorized=%v exit_ip=%s tz=%s reason=%s",
				rep.Authorized, rep.ExitIP, rep.SyncedTZ, rep.BlockReason)

			interval := 10 * time.Minute
			if !rep.Authorized {
				interval = 20 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("[relay] listening on %s (accounts=%s, proxy=%s)", listenAddr, accountsDir, proxyURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("relay server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func loadOrDefaultPolicy(path string) exitpolicy.Policy {
	if b, err := os.ReadFile(path); err == nil {
		var p exitpolicy.Policy
		if json.Unmarshal(b, &p) == nil && p.Validate() == nil {
			return p.Normalized()
		}
	}
	p := exitpolicy.Policy{
		Mode:        envOr("EXIT_MODE", "dynamic"),
		Scope:       envOr("EXIT_SCOPE", "any"),
		IP:          strings.TrimSpace(os.Getenv("EXIT_IP")),
		CountryCode: strings.TrimSpace(os.Getenv("EXIT_COUNTRY")),
	}.Normalized()
	if p.Validate() != nil {
		p = exitpolicy.DefaultPolicy()
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	return p
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
