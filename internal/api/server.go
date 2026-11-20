package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mini-cage-relay/internal/accounts"
	"mini-cage-relay/internal/claude"
	"mini-cage-relay/internal/exitpolicy"
	"mini-cage-relay/internal/mihomo"
	"mini-cage-relay/internal/safety"
	"mini-cage-relay/internal/updater"
	"mini-cage-relay/internal/webui"
)

type ServerConfig struct {
	APIKey            string
	ProxyURL          string
	GatewayControlURL string
	InternalToken     string
	PolicyFile        string
}

type Server struct {
	cfg        ServerConfig
	checker    *safety.Checker
	pool       *accounts.Pool
	runner     *claude.Runner
	updater    *updater.Checker
	httpClient *http.Client
	mux        *http.ServeMux
}

func NewServer(cfg ServerConfig, checker *safety.Checker, pool *accounts.Pool, runner *claude.Runner) *Server {
	s := &Server{
		cfg:        cfg,
		checker:    checker,
		pool:       pool,
		runner:     runner,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		mux:        http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) SetUpdater(u *updater.Checker) {
	s.updater = u
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleUI)
	s.mux.HandleFunc("/ui", s.handleUI)
	s.mux.HandleFunc("/health", s.handleHealth)

	// OpenAI & Anthropic compatible APIs (protected by APIKey + Fail-Closed Safety Gate)
	s.mux.HandleFunc("/v1/models", s.requireAuth(s.handleOpenAIModels))
	s.mux.HandleFunc("/v1/chat/completions", s.requireAuth(s.handleOpenAIChat))
	s.mux.HandleFunc("/v1/messages", s.requireAuth(s.handleAnthropicMessages))

	// Local Admin APIs for Web UI
	s.mux.HandleFunc("/admin/status", s.requireAuth(s.handleAdminStatus))
	s.mux.HandleFunc("/admin/check", s.requireAuth(s.handleAdminCheck))
	s.mux.HandleFunc("/admin/proxy/subscription", s.requireAuth(s.handleAdminProxySub))
	s.mux.HandleFunc("/admin/proxy/static", s.requireAuth(s.handleAdminProxyStatic))
	s.mux.HandleFunc("/admin/proxy/select", s.requireAuth(s.handleAdminProxySelect))
	s.mux.HandleFunc("/admin/proxy/delay", s.requireAuth(s.handleAdminProxyDelay))
	s.mux.HandleFunc("/admin/policy", s.requireAuth(s.handleAdminPolicy))
	s.mux.HandleFunc("/admin/accounts", s.requireAuth(s.handleAdminAccounts))
	s.mux.HandleFunc("/admin/accounts/toggle", s.requireAuth(s.handleAdminAccountToggle))
	s.mux.HandleFunc("/admin/accounts/usage", s.requireAuth(s.handleAdminAccountUsage))
	s.mux.HandleFunc("/admin/browser/open", s.requireAuth(s.handleAdminBrowserOpen))
	s.mux.HandleFunc("/admin/updates/check", s.requireAuth(s.handleAdminUpdatesCheck))
	s.mux.HandleFunc("/admin/updates/claude-code", s.requireAuth(s.handleAdminUpdateClaudeCode))
	s.mux.HandleFunc("/admin/hostexec/test", s.requireAuth(s.handleAdminHostExecTest))
	s.mux.HandleFunc("/admin/hostexec/add-tool", s.requireAuth(s.handleAdminHostExecAddTool))
	s.mux.HandleFunc("/admin/hostexec/install-tool", s.requireAuth(s.handleAdminHostExecInstallTool))
	s.mux.HandleFunc("/admin/hostexec/policy", s.requireAuth(s.handleAdminHostExecPolicy))
	s.mux.HandleFunc("/admin/hostexec/unproxied/clear", s.requireAuth(s.handleAdminUnproxiedClear))
	s.mux.HandleFunc("/admin/vm/apply", s.requireAuth(s.handleAdminVMApply))
	s.mux.HandleFunc("/admin/vm/restart", s.requireAuth(s.handleAdminVMRestart))
	s.mux.HandleFunc("/admin/resume/status", s.requireAuth(s.handleAdminResumeStatus))
	s.mux.HandleFunc("/admin/resume/run", s.requireAuth(s.handleAdminResumeRun))
	s.mux.HandleFunc("/admin/resume/stop", s.requireAuth(s.handleAdminResumeStop))
	s.mux.HandleFunc("/admin/resume/schedule", s.requireAuth(s.handleAdminResumeSchedule))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/ui" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(webui.IndexHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rep := s.checker.Report()
	status := http.StatusOK
	if !rep.Authorized {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"authorized":   rep.Authorized,
		"block_reason": rep.BlockReason,
		"exit_ip":      rep.ExitIP,
		"synced_tz":    rep.SyncedTZ,
		"checked_at":   rep.CreatedAt,
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected := strings.TrimSpace(s.cfg.APIKey)
		if expected == "" {
			next(w, r)
			return
		}
		got := ""
		if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			got = strings.TrimSpace(auth[7:])
		} else if xkey := strings.TrimSpace(r.Header.Get("x-api-key")); xkey != "" {
			got = xkey
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "未授权：请在请求头提供正确的 Bearer / x-api-key (RELAY_API_KEY)")
			return
		}
		next(w, r)
	}
}

func (s *Server) executeWithFailover(
	ctx context.Context,
	rep safety.Report,
	model, sysPrompt string,
	msgs []claude.Message,
	onDelta func(string) error,
) (claude.RunResult, string, error) {
	tried := make(map[string]bool)
	const maxAttempts = 3

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, err := s.pool.Acquire(tried)
		if err != nil {
			if lastErr != nil {
				return claude.RunResult{}, "", lastErr
			}
			return claude.RunResult{}, "", err
		}
		tried[acc.ID] = true

		emittedAny := false
		wrappedDelta := onDelta
		if onDelta != nil {
			wrappedDelta = func(d string) error {
				emittedAny = true
				return onDelta(d)
			}
		}

		res, runErr := s.runner.Run(ctx, claude.RunRequest{
			Model:        model,
			SystemPrompt: sysPrompt,
			Messages:     msgs,
			AccountDir:   acc.ConfigDir,
			ProxyURL:     s.cfg.ProxyURL,
			SyncedTZ:     rep.SyncedTZ,
			SyncedLocale: rep.SyncedLocale,
			OnTextDelta:  wrappedDelta,
		})
		s.pool.Release(acc.ID)

		if runErr == nil {
			s.pool.ClearError(acc.ID)
			return res, acc.ID, nil
		}

		lastErr = runErr
		var rlErr *claude.RateLimitError
		if errors.As(runErr, &rlErr) {
			s.pool.MarkRateLimited(acc.ID, 15*time.Minute, rlErr.Message)
			// Only failover cleanly if we have not already streamed partial tokens to the client.
			if !emittedAny {
				continue
			}
		}
		return res, acc.ID, runErr
	}
	return claude.RunResult{}, "", lastErr
}

func (s *Server) buildAdminPayload(ctx context.Context) map[string]any {
	gw, _ := s.FetchGatewayStatus(ctx)
	payload := map[string]any{
		"report":   s.checker.Report(),
		"policy":   s.checker.Policy(),
		"gateway":  gw,
		"accounts": s.pool.List(),
	}
	if s.updater != nil {
		payload["updates"] = s.updater.Report()
	}
	payload["hostexec"] = s.fetchHostExecStatus(ctx)
	payload["vm"] = s.fetchVMStatus(ctx)
	payload["resume"] = s.fetchResumeStatus(ctx)
	payload["unproxied_cmds"] = readUnproxiedCommands("/data/runtime/unproxied_cmds.jsonl")
	return payload
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	s.checker.RunAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminProxySub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if _, err := s.callGateway(r.Context(), http.MethodPost, "/internal/proxy/subscription", body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checker.RunAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminProxyStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if _, err := s.callGateway(r.Context(), http.MethodPost, "/internal/proxy/static", body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checker.RunAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminProxySelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if _, err := s.callGateway(r.Context(), http.MethodPost, "/internal/proxy/select", body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.checker.RunAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminProxyDelay(w http.ResponseWriter, r *http.Request) {
	node := strings.TrimSpace(r.URL.Query().Get("node"))
	if node == "" {
		writeOpenAIError(w, http.StatusBadRequest, "缺少 node 参数")
		return
	}
	respBytes, err := s.callGateway(r.Context(), http.MethodGet, "/internal/proxy/delay?node="+url.QueryEscape(node), nil)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBytes)
}

func (s *Server) handleAdminPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req struct {
		BindCurrent bool              `json:"bind_current"`
		Policy      exitpolicy.Policy `json:"policy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	pol := req.Policy.Normalized()
	if req.BindCurrent {
		rep := s.checker.Report()
		if rep.Location == nil {
			writeOpenAIError(w, http.StatusBadRequest, "当前尚未探测到有效出口位置，请先确保代理可用并点击检测")
			return
		}
		pol = pol.BindFrom(*rep.Location)
	}
	if err := s.checker.SetPolicy(pol); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.cfg.PolicyFile != "" {
		_ = os.MkdirAll(filepath.Dir(s.cfg.PolicyFile), 0o700)
		if b, err := json.MarshalIndent(pol, "", "  "); err == nil {
			_ = os.WriteFile(s.cfg.PolicyFile, b, 0o600)
		}
	}
	s.checker.RunAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST/GET required")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := s.pool.Ensure(req.ID)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) handleAdminAccountToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.pool.SetEnabled(req.ID, req.Enabled); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.pool.List()})
}

func (s *Server) handleAdminAccountUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	rep, err := s.checker.CheckAuthorized(time.Now().UTC())
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "安全门控未放行，暂不查询用量: "+err.Error())
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)

	var targets []accounts.Account
	if id := strings.TrimSpace(req.ID); id != "" {
		acc, ok := s.pool.Get(id)
		if !ok {
			writeOpenAIError(w, http.StatusNotFound, "未找到账号 "+id)
			return
		}
		targets = []accounts.Account{*acc}
	} else {
		for _, a := range s.pool.List() {
			if a.LoggedIn {
				targets = append(targets, a)
			}
		}
	}

	for _, acc := range targets {
		if !acc.LoggedIn {
			continue
		}
		u, _ := accounts.QueryUsage(r.Context(), s.runner.BinaryPath, acc.ConfigDir, s.cfg.ProxyURL, rep.SyncedTZ, rep.SyncedLocale)
		if u != nil {
			s.pool.SetUsage(acc.ID, u)
		}
	}
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) handleAdminBrowserOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	target := strings.TrimSpace(req.URL)
	if target == "" {
		target = "https://claude.ai"
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}
	triggerFile := "/data/runtime/browser_open_url"
	if err := os.MkdirAll(filepath.Dir(triggerFile), 0o755); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(triggerFile, []byte(target+"\n"), 0o666); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": target})
}

func (s *Server) handleAdminUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if s.updater == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "更新检测模块未初始化")
		return
	}
	s.updater.CheckAll(r.Context())
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}


func (s *Server) handleAdminUpdateClaudeCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if s.updater == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "更新检测模块未初始化")
		return
	}
	if _, err := s.checker.CheckAuthorized(time.Now().UTC()); err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "请先确保安全门控放行后再在线升级: "+err.Error())
		return
	}
	logOut, err := s.updater.UpdateClaudeCode(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error()+" | "+logOut)
		return
	}
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) hostExecBaseURL() string {
	u := strings.TrimSpace(os.Getenv("HOSTEXEC_URL"))
	if u == "" {
		u = "http://gateway:18082/exec"
	}
	return strings.TrimSuffix(strings.TrimRight(u, "/"), "/exec")
}

func (s *Server) hostExecToken() string {
	if t := strings.TrimSpace(os.Getenv("HOSTEXEC_TOKEN")); t != "" {
		return t
	}
	return s.cfg.InternalToken
}

func (s *Server) fetchHostExecStatus(ctx context.Context) map[string]any {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.hostExecBaseURL()+"/status", nil)
	if err != nil {
		return map[string]any{"online": false, "error": err.Error()}
	}
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return map[string]any{"online": false, "error": "宿主机 hostexec 守护进程未运行或不可达"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return map[string]any{"online": false, "error": fmt.Sprintf("hostexec 返回 HTTP %d", resp.StatusCode)}
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&out); err != nil {
		return map[string]any{"online": false, "error": err.Error()}
	}
	return out
}

func (s *Server) handleAdminHostExecTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var bodyReq struct {
		Cmd string `json:"cmd"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&bodyReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	cmdName := strings.TrimSpace(bodyReq.Cmd)
	if cmdName == "" {
		cmdName = "cargo"
	}
	args := []string{"--version"}
	if cmdName == "screencapture" {
		args = []string{"-h"}
	}
	started := time.Now()
	payload, _ := json.Marshal(map[string]any{"cmd": cmdName, "args": args, "cwd": ""})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.hostExecBaseURL()+"/exec", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		writeOpenAIError(w, http.StatusBadGateway, strings.TrimSpace(string(raw)))
		return
	}
	var outText strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		var ev struct {
			Data string `json:"data"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Data != "" {
			outText.WriteString(ev.Data)
		}
	}
	firstLine := strings.TrimSpace(strings.Split(strings.TrimSpace(outText.String()), "\n")[0])
	writeJSON(w, http.StatusOK, map[string]any{
		"cmd":         cmdName,
		"output":      firstLine,
		"duration_ms": time.Since(started).Milliseconds(),
		"hostexec":    s.fetchHostExecStatus(r.Context()),
	})
}

type unproxiedSummary struct {
	Bin     string `json:"bin"`
	Count   int    `json:"count"`
	Missing bool   `json:"missing"`
	LastCmd string `json:"last_cmd"`
	LastTS  string `json:"last_ts"`
	Source  string `json:"source"`
}

func readUnproxiedCommands(path string) []unproxiedSummary {
	b, err := os.ReadFile(path)
	if err != nil {
		return []unproxiedSummary{}
	}
	byBin := make(map[string]*unproxiedSummary)
	var order []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			TS      string `json:"ts"`
			Bin     string `json:"bin"`
			Cmd     string `json:"cmd"`
			Missing bool   `json:"missing"`
			Source  string `json:"source"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Bin == "" {
			continue
		}
		// Hide if user has since added a dynamic shim for this binary.
		if _, err := os.Stat(filepath.Join("/data/runtime/shims", rec.Bin)); err == nil {
			continue
		}
		cur, ok := byBin[rec.Bin]
		if !ok {
			cur = &unproxiedSummary{Bin: rec.Bin}
			byBin[rec.Bin] = cur
			order = append(order, rec.Bin)
		}
		cur.Count++
		if rec.Missing {
			cur.Missing = true
		}
		cur.LastCmd = rec.Cmd
		cur.LastTS = rec.TS
		cur.Source = rec.Source
	}
	out := make([]unproxiedSummary, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		out = append(out, *byBin[order[i]])
	}
	return out
}

func (s *Server) handleAdminHostExecAddTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var bodyReq struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&bodyReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(bodyReq.Name)
	if name == "" || strings.ContainsAny(name, "/\\ \t\n\r;|&$`") {
		writeOpenAIError(w, http.StatusBadRequest, "无效命令名")
		return
	}
	payload, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.hostExecBaseURL()+"/tools/add", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		writeOpenAIError(w, http.StatusBadRequest, strings.TrimSpace(string(raw)))
		return
	}
	// Create shared dynamic shim symlink so BOTH relay & desktop immediately see it on PATH!
	_ = os.MkdirAll("/data/runtime/shims", 0o755)
	linkPath := filepath.Join("/data/runtime/shims", name)
	_ = os.Remove(linkPath)
	if err := os.Symlink("/usr/local/bin/hostshim", linkPath); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "创建共享代理软链失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) handleAdminHostExecInstallTool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var bodyReq struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&bodyReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(bodyReq.Name)
	if name == "" || strings.ContainsAny(name, "/\\ \t\n\r;|&$`") {
		writeOpenAIError(w, http.StatusBadRequest, "无效命令名")
		return
	}
	payload, _ := json.Marshal(map[string]string{"name": name})
	installCtx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(installCtx, http.MethodPost, s.hostExecBaseURL()+"/tools/install", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	client := &http.Client{Timeout: 4 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		writeOpenAIError(w, http.StatusBadRequest, strings.TrimSpace(string(raw)))
		return
	}
	// Also ensure shared shim symlink exists immediately
	_ = os.MkdirAll("/data/runtime/shims", 0o755)
	linkPath := filepath.Join("/data/runtime/shims", name)
	_ = os.Remove(linkPath)
	_ = os.Symlink("/usr/local/bin/hostshim", linkPath)
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) handleAdminHostExecPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.hostExecBaseURL()+"/tools/policy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) handleAdminUnproxiedClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	_ = os.Remove("/data/runtime/unproxied_cmds.jsonl")
	writeJSON(w, http.StatusOK, s.buildAdminPayload(r.Context()))
}

func (s *Server) fetchVMStatus(ctx context.Context) map[string]any {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.hostExecBaseURL()+"/vm/status", nil)
	if err != nil {
		return nil
	}
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&out) != nil {
		return nil
	}
	return out
}

func (s *Server) proxyHostExecPost(w http.ResponseWriter, r *http.Request, subpath string) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, s.hostExecBaseURL()+subpath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode >= 300 {
		writeOpenAIError(w, resp.StatusCode, strings.TrimSpace(string(raw)))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func (s *Server) handleAdminVMApply(w http.ResponseWriter, r *http.Request) {
	s.proxyHostExecPost(w, r, "/vm/apply")
}

func (s *Server) handleAdminVMRestart(w http.ResponseWriter, r *http.Request) {
	s.proxyHostExecPost(w, r, "/vm/restart")
}

func (s *Server) fetchResumeStatus(ctx context.Context) map[string]any {
	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.hostExecBaseURL()+"/resume/status", nil)
	if err != nil {
		return nil
	}
	if tok := s.hostExecToken(); tok != "" {
		req.Header.Set("X-HostExec-Token", tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&out) != nil {
		return nil
	}
	return out
}

func (s *Server) handleAdminResumeStatus(w http.ResponseWriter, r *http.Request) {
	st := s.fetchResumeStatus(r.Context())
	if st == nil {
		writeOpenAIError(w, http.StatusBadGateway, "宿主机 hostexec 未在线")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAdminResumeRun(w http.ResponseWriter, r *http.Request) {
	s.proxyHostExecPost(w, r, "/resume/run")
}

func (s *Server) handleAdminResumeStop(w http.ResponseWriter, r *http.Request) {
	s.proxyHostExecPost(w, r, "/resume/stop")
}

func (s *Server) handleAdminResumeSchedule(w http.ResponseWriter, r *http.Request) {
	s.proxyHostExecPost(w, r, "/resume/schedule")
}

func (s *Server) FetchGatewayStatus(ctx context.Context) (*mihomo.Status, error) {
	b, err := s.callGateway(ctx, http.MethodGet, "/internal/status", nil)
	if err != nil {
		return nil, err
	}
	var st mihomo.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Server) callGateway(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if strings.TrimSpace(s.cfg.GatewayControlURL) == "" {
		return nil, errors.New("未配置 RELAY_GATEWAY_CONTROL_URL")
	}
	endpoint := strings.TrimRight(s.cfg.GatewayControlURL, "/") + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.InternalToken != "" {
		req.Header.Set("X-Internal-Token", s.cfg.InternalToken)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var errObj struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBytes, &errObj) == nil && errObj.Error != "" {
			return nil, errors.New(errObj.Error)
		}
		return nil, fmt.Errorf("gateway 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	return respBytes, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
