package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mini-cage-relay/internal/mihomo"
)

func main() {
	dataDir := envOr("GATEWAY_DATA_DIR", "/data/gateway")
	mihomoBin := envOr("MIHOMO_BIN", "mihomo")
	relayUpstream := envOr("RELAY_UPSTREAM", "http://relay:8317")
	loginUpstream := envOr("LOGIN_UPSTREAM", "http://desktop:6080")
	termUpstream := envOr("TERMINAL_UPSTREAM", "http://relay:7681")
	browserUpstream := envOr("BROWSER_UPSTREAM", "http://desktop:6081")
	clipUpstream := envOr("CLIPBOARD_UPSTREAM", "http://desktop:6082")
	hostExecUpstream := envOr("HOSTEXEC_UPSTREAM", "http://host.docker.internal:8325")
	internalToken := strings.TrimSpace(os.Getenv("INTERNAL_TOKEN"))

	mgr := mihomo.NewManager(dataDir, mihomoBin, 7890, "127.0.0.1:9090")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bootstrap from persisted config first; if unconfigured, apply optional env vars.
	if err := mgr.StartIfConfigured(ctx); err != nil {
		log.Printf("[gateway] persisted mihomo start warning: %v", err)
	}
	if mgr.Status().Mode == "unconfigured" {
		if subURL := strings.TrimSpace(os.Getenv("SUBSCRIPTION_URL")); subURL != "" {
			landing := strings.TrimSpace(os.Getenv("LANDING_PROXY_URL"))
			if _, err := mgr.ApplySubscription(ctx, subURL, "", landing); err != nil {
				log.Printf("[gateway] initial subscription apply failed: %v", err)
			} else {
				log.Printf("[gateway] applied initial subscription from SUBSCRIPTION_URL")
			}
		} else if staticURL := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")); staticURL != "" {
			if _, err := mgr.ApplyStaticProxy(ctx, staticURL); err != nil {
				log.Printf("[gateway] initial static proxy apply failed: %v", err)
			} else {
				log.Printf("[gateway] applied initial static proxy from UPSTREAM_PROXY")
			}
		} else {
			log.Printf("[gateway] no proxy configured yet; open http://127.0.0.1:8317/ui to configure egress")
		}
	}

	// 1. Public-to-host ingress reverse proxy (:8317) — unified SAME-ORIGIN mux so iframes have full clipboard access!
	relayTarget, err := url.Parse(relayUpstream)
	if err != nil {
		log.Fatalf("invalid RELAY_UPSTREAM: %v", err)
	}
	relayProxy := httputil.NewSingleHostReverseProxy(relayTarget)
	relayProxy.FlushInterval = -1
	relayProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "relay 容器正在启动或不可达，请稍候重试: " + err.Error(),
		})
	}

	// 2. Desktop noVNC reverse proxy (:8318 -> relay:6080, also mounted same-origin at /desktop/ on :8317).
	loginTarget, err := url.Parse(loginUpstream)
	if err != nil {
		log.Fatalf("invalid LOGIN_UPSTREAM: %v", err)
	}
	loginProxy := httputil.NewSingleHostReverseProxy(loginTarget)
	loginProxy.FlushInterval = -1
	loginProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Claude 桌面端正在启动中，请稍候刷新...\n"))
	}
	loginSrv := &http.Server{
		Addr:              ":8318",
		Handler:           loginProxy,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 2b. Built-in container Web Terminal reverse proxy (:8319 -> relay:7681).
	termTarget, err := url.Parse(termUpstream)
	if err != nil {
		log.Fatalf("invalid TERMINAL_UPSTREAM: %v", err)
	}
	termProxy := httputil.NewSingleHostReverseProxy(termTarget)
	termProxy.FlushInterval = -1
	termProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("容器终端正在启动中，请稍候刷新...\n"))
	}
	termSrv := &http.Server{
		Addr:              ":8319",
		Handler:           termProxy,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 2c. Dedicated isolated Chromium browser noVNC reverse proxy (:8320 -> relay:6081, also at /browser/ on :8317).
	browserTarget, err := url.Parse(browserUpstream)
	if err != nil {
		log.Fatalf("invalid BROWSER_UPSTREAM: %v", err)
	}
	browserProxy := httputil.NewSingleHostReverseProxy(browserTarget)
	browserProxy.FlushInterval = -1
	browserProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("隔离浏览器正在启动中，请稍候刷新...\n"))
	}
	browserSrv := &http.Server{
		Addr:              ":8320",
		Handler:           browserProxy,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 2d. Same-origin UTF-8 X11 clipboard bridge (/clipboard -> relay:6082).
	clipTarget, err := url.Parse(clipUpstream)
	if err != nil {
		log.Fatalf("invalid CLIPBOARD_UPSTREAM: %v", err)
	}
	clipProxy := httputil.NewSingleHostReverseProxy(clipTarget)
	clipProxy.FlushInterval = -1

	ingressMux := http.NewServeMux()
	ingressMux.Handle("/desktop/", http.StripPrefix("/desktop", loginProxy))
	ingressMux.Handle("/browser/", http.StripPrefix("/browser", browserProxy))
	ingressMux.Handle("/clipboard", clipProxy)
	ingressMux.Handle("/", relayProxy)

	ingressSrv := &http.Server{
		Addr:              ":8317",
		Handler:           ingressMux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 3. Internal control API (:18080, reachable only inside cage_internal).
	ctrlMux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if internalToken != "" {
				got := r.Header.Get("X-Internal-Token")
				if subtle.ConstantTimeCompare([]byte(got), []byte(internalToken)) != 1 {
					writeErr(w, http.StatusUnauthorized, "unauthorized internal token")
					return
				}
			}
			next(w, r)
		}
	}
	ctrlMux.HandleFunc("/internal/status", auth(func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, mgr.Status())
	}))
	ctrlMux.HandleFunc("/internal/proxy/static", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ProxyURL string `json:"proxy_url"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := mgr.ApplyStaticProxy(r.Context(), req.ProxyURL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOK(w, st)
	}))
	ctrlMux.HandleFunc("/internal/proxy/subscription", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SubscriptionURL string `json:"subscription_url"`
			RawYAML         string `json:"raw_yaml"`
			LandingProxyURL string `json:"landing_proxy_url"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := mgr.ApplySubscription(r.Context(), req.SubscriptionURL, req.RawYAML, req.LandingProxyURL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOK(w, st)
	}))
	ctrlMux.HandleFunc("/internal/proxy/select", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Node string `json:"node"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := mgr.SelectNode(r.Context(), req.Node)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOK(w, st)
	}))
	ctrlMux.HandleFunc("/internal/proxy/delay", auth(func(w http.ResponseWriter, r *http.Request) {
		node := r.URL.Query().Get("node")
		delay, err := mgr.TestDelay(r.Context(), node)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeOK(w, map[string]any{"node": node, "delay_ms": delay})
	}))

	ctrlSrv := &http.Server{
		Addr:              ":18080",
		Handler:           ctrlMux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// 4. Single-target authenticated hostexec bridge (:18082 -> host.docker.internal:8325, internal only).
	hostExecTarget, err := url.Parse(hostExecUpstream)
	if err != nil {
		log.Fatalf("invalid HOSTEXEC_UPSTREAM: %v", err)
	}
	hostExecProxy := httputil.NewSingleHostReverseProxy(hostExecTarget)
	hostExecProxy.FlushInterval = -1
	hostExecProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("宿主机 hostexec 守护进程未运行，请在宿主机运行 ./scripts/hostexec.sh (或 ./scripts/start.sh): " + err.Error() + "\n"))
	}
	hostExecSrv := &http.Server{
		Addr:              ":18082",
		Handler:           hostExecProxy,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("[gateway] ingress listening on :8317 -> %s", relayUpstream)
		if err := ingressSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ingress server failed: %v", err)
		}
	}()
	go func() {
		log.Printf("[gateway] desktop noVNC proxy listening on :8318 -> %s", loginUpstream)
		if err := loginSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("desktop proxy server failed: %v", err)
		}
	}()
	go func() {
		log.Printf("[gateway] terminal proxy listening on :8319 -> %s", termUpstream)
		if err := termSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("terminal proxy server failed: %v", err)
		}
	}()
	go func() {
		log.Printf("[gateway] browser noVNC proxy listening on :8320 -> %s", browserUpstream)
		if err := browserSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("browser proxy server failed: %v", err)
		}
	}()
	go func() {
		log.Printf("[gateway] internal control listening on :18080")
		if err := ctrlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("control server failed: %v", err)
		}
	}()
	go func() {
		log.Printf("[gateway] hostexec bridge listening on :18082 -> %s", hostExecUpstream)
		if err := hostExecSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("hostexec bridge server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ingressSrv.Shutdown(shutCtx)
	_ = loginSrv.Shutdown(shutCtx)
	_ = termSrv.Shutdown(shutCtx)
	_ = browserSrv.Shutdown(shutCtx)
	_ = ctrlSrv.Shutdown(shutCtx)
	_ = hostExecSrv.Shutdown(shutCtx)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
