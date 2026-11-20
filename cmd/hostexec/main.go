package main

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type execRequest struct {
	Cmd      string            `json:"cmd"`
	Args     []string          `json:"args"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
}

type streamEvent struct {
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type toolStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	HostPath  string `json:"host_path,omitempty"`
}

type runRecord struct {
	ID         int64     `json:"id"`
	Cmd        string    `json:"cmd"`
	Args       []string  `json:"args"`
	GuestCwd   string    `json:"guest_cwd"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	Running    bool      `json:"running"`
	ExitCode   int       `json:"exit_code"`
}

type auditTracker struct {
	mu      sync.Mutex
	nextID  int64
	total   int64
	records []runRecord
}

func (a *auditTracker) start(cmd string, args []string, guestCwd string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	a.total++
	rec := runRecord{
		ID:        a.nextID,
		Cmd:       cmd,
		Args:      append([]string(nil), args...),
		GuestCwd:  guestCwd,
		StartedAt: time.Now().UTC(),
		Running:   true,
	}
	a.records = append([]runRecord{rec}, a.records...)
	if len(a.records) > 25 {
		a.records = a.records[:25]
	}
	return rec.ID
}

func (a *auditTracker) finish(id int64, exitCode int, dur time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.records {
		if a.records[i].ID == id {
			a.records[i].Running = false
			a.records[i].ExitCode = exitCode
			a.records[i].DurationMs = dur.Milliseconds()
			break
		}
	}
}

func (a *auditTracker) snapshot() (int64, []runRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]runRecord, len(a.records))
	copy(out, a.records)
	return a.total, out
}

type pathMount struct {
	Guest string
	Host  string
}

type hostConfig struct {
	mu             sync.RWMutex
	gitWriteMu     sync.Mutex
	ListenAddr     string
	Token          string
	WorkspaceHost  string
	WorkspaceGuest string
	Mounts         []pathMount // ordered longest-prefix-first
	ExtraToolsFile string
	AllowedTools   map[string]bool
	Tracker        *auditTracker
}

func (c *hostConfig) isAllowed(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AllowedTools[name]
}

func (c *hostConfig) toolNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return mapKeys(c.AllowedTools)
}

func (c *hostConfig) addTool(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\ \t\n\r;|&$`") {
		return errors.New("无效命令名称")
	}
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("宿主机 PATH 中未找到命令 %q", name)
	}
	c.mu.Lock()
	c.AllowedTools[name] = true
	var extras []string
	for k := range c.AllowedTools {
		extras = append(extras, k)
	}
	sort.Strings(extras)
	extraFile := c.ExtraToolsFile
	c.mu.Unlock()
	if extraFile != "" {
		_ = os.MkdirAll(filepath.Dir(extraFile), 0o755)
		if b, err := json.MarshalIndent(extras, "", "  "); err == nil {
			_ = os.WriteFile(extraFile, b, 0o644)
		}
	}
	return nil
}

func main() {
	cfg := loadConfig()
	if cfg.WorkspaceHost == "" {
		log.Fatalf("[hostexec] WORKSPACE_DIR 未配置")
	}
	initResumeScheduler()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":              true,
			"workspace_guest": cfg.WorkspaceGuest,
		})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		handleStatus(w, r, &cfg)
	})
	mux.HandleFunc("/tools/add", func(w http.ResponseWriter, r *http.Request) {
		handleAddTool(w, r, &cfg)
	})
	mux.HandleFunc("/vm/status", func(w http.ResponseWriter, r *http.Request) {
		handleVMStatus(w, r, &cfg)
	})
	mux.HandleFunc("/vm/apply", func(w http.ResponseWriter, r *http.Request) {
		handleVMApply(w, r, &cfg)
	})
	mux.HandleFunc("/vm/restart", func(w http.ResponseWriter, r *http.Request) {
		handleVMRestart(w, r, &cfg)
	})
	mux.HandleFunc("/resume/status", func(w http.ResponseWriter, r *http.Request) {
		handleResumeStatus(w, r, &cfg)
	})
	mux.HandleFunc("/resume/run", func(w http.ResponseWriter, r *http.Request) {
		handleResumeRun(w, r, &cfg)
	})
	mux.HandleFunc("/resume/stop", func(w http.ResponseWriter, r *http.Request) {
		handleResumeStop(w, r, &cfg)
	})
	mux.HandleFunc("/resume/schedule", func(w http.ResponseWriter, r *http.Request) {
		handleResumeSchedule(w, r, &cfg)
	})
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		handleExec(w, r, &cfg)
	})

	log.Printf("[hostexec] listening on %s (guest=%s -> host=%s, mounts=%d, tools=%v)",
		cfg.ListenAddr, cfg.WorkspaceGuest, cfg.WorkspaceHost, len(cfg.Mounts), cfg.toolNames())
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		log.Fatalf("[hostexec] server failed: %v", err)
	}
}

func loadConfig() hostConfig {
	listen := envOr("HOSTEXEC_ADDR", "127.0.0.1:8325")
	token := strings.TrimSpace(os.Getenv("HOSTEXEC_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("INTERNAL_TOKEN"))
	}
	wsHost, _ := filepath.Abs(envOr("WORKSPACE_DIR", "./workspace"))
	wsName := envOr("WORKSPACE_NAME", "project")
	wsGuest := "/workspace/" + strings.Trim(wsName, "/")

	desktopHost, _ := filepath.Abs(envOr("DESKTOP_DATA_DIR", "./data/desktop"))
	sharedTmpHost, _ := filepath.Abs(envOr("SHARED_TMP_DIR", "./data/shared-tmp"))
	_ = os.MkdirAll(sharedTmpHost, 0o755)

	mounts := []pathMount{
		{Guest: "/data/desktop" + wsGuest, Host: wsHost},
		{Guest: wsGuest, Host: wsHost},
		{Guest: "/data/desktop", Host: desktopHost},
		{Guest: "/tmp/edualc", Host: sharedTmpHost},
	}

	rawTools := envOr("HOSTEXEC_ALLOW_TOOLS", "cargo,rustc,rustup,just,make,git,bun,rg,screencapture,agy,codex")
	allowed := make(map[string]bool)
	for _, t := range strings.Split(rawTools, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			allowed[t] = true
		}
	}
	extraFile := envOr("HOSTEXEC_EXTRA_TOOLS_FILE", "data/runtime/extra_host_tools.json")
	if b, err := os.ReadFile(extraFile); err == nil {
		var persisted []string
		if json.Unmarshal(b, &persisted) == nil {
			for _, t := range persisted {
				t = strings.TrimSpace(t)
				if t != "" {
					allowed[t] = true
				}
			}
		}
	}
	return hostConfig{
		ListenAddr:     listen,
		Token:          token,
		WorkspaceHost:  wsHost,
		WorkspaceGuest: wsGuest,
		Mounts:         mounts,
		ExtraToolsFile: extraFile,
		AllowedTools:   allowed,
		Tracker:        &auditTracker{},
	}
}

func checkToken(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("X-HostExec-Token"))
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func handleStatus(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	names := cfg.toolNames()
	sort.Strings(names)
	tools := make([]toolStatus, 0, len(names))
	availCount := 0
	for _, name := range names {
		p, err := exec.LookPath(name)
		ok := err == nil && p != ""
		if ok {
			availCount++
		}
		tools = append(tools, toolStatus{
			Name:      name,
			Available: ok,
			HostPath:  p,
		})
	}
	totalRuns, recent := cfg.Tracker.snapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"online":          true,
		"listen_addr":     cfg.ListenAddr,
		"workspace_guest": cfg.WorkspaceGuest,
		"workspace_host":  cfg.WorkspaceHost,
		"available_count": availCount,
		"total_tools":     len(tools),
		"total_runs":      totalRuns,
		"tools":           tools,
		"recent_runs":     recent,
	})
}

func handleAddTool(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := cfg.addTool(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	handleStatus(w, r, cfg)
}

func mapGuestToHostCwd(guestCwd string, mounts []pathMount, defaultHost string) (string, error) {
	guestCwd = strings.TrimSpace(guestCwd)
	if guestCwd == "" || guestCwd == "/workspace" {
		return defaultHost, nil
	}
	for _, m := range mounts {
		if guestCwd == m.Guest {
			return m.Host, nil
		}
		if strings.HasPrefix(guestCwd, m.Guest+"/") {
			rel := strings.TrimPrefix(guestCwd, m.Guest+"/")
			cleaned := filepath.Clean(filepath.FromSlash(rel))
			if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
				return "", errors.New("工作目录越界，已拒绝执行")
			}
			target := filepath.Join(m.Host, cleaned)
			if target != m.Host && !strings.HasPrefix(target, m.Host+string(filepath.Separator)) {
				return "", errors.New("工作目录越界，已拒绝执行")
			}
			return target, nil
		}
	}
	return defaultHost, nil
}

func rewriteGuestToHost(s string, mounts []pathMount) string {
	for _, m := range mounts {
		if m.Guest != "" && m.Host != "" {
			s = strings.ReplaceAll(s, m.Guest, m.Host)
		}
	}
	return s
}

func rewriteArgsGuestToHost(args []string, mounts []pathMount) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = rewriteGuestToHost(a, mounts)
	}
	return out
}

func scrubHostToGuest(text string, mounts []pathMount) string {
	if text == "" {
		return text
	}
	// Replace specific mounts (skip duplicate /data/desktop/workspace alias, prefer canonical Guest path)
	seenHost := make(map[string]bool)
	for _, m := range mounts {
		if m.Host == "" || strings.HasPrefix(m.Guest, "/data/desktop/workspace") {
			continue
		}
		if seenHost[m.Host] {
			continue
		}
		seenHost[m.Host] = true
		text = strings.ReplaceAll(text, m.Host, m.Guest)
	}
	return text
}

var allowedEnvPrefixes = []string{
	"CARGO_", "RUST", "LK2_", "BEVY_", "NEXTEST_", "WGPU_", "RAYON_",
}
var allowedEnvExact = map[string]bool{
	"NO_COLOR": true, "TERM": true, "CI": true, "COLORTERM": true, "TMPDIR": true,
}

func mergeSafeEnv(base []string, extra map[string]string, mounts []pathMount) []string {
	if len(extra) == 0 {
		return base
	}
	envMap := make(map[string]string, len(base)+len(extra))
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}
	for k, v := range extra {
		ok := allowedEnvExact[k]
		if !ok {
			for _, pfx := range allowedEnvPrefixes {
				if strings.HasPrefix(k, pfx) {
					ok = true
					break
				}
			}
		}
		if !ok {
			continue
		}
		envMap[k] = rewriteGuestToHost(v, mounts)
	}
	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	return out
}

var gitReadOnlySubs = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "rev-parse": true,
	"ls-files": true, "grep": true, "describe": true, "cat-file": true,
	"for-each-ref": true, "merge-base": true, "blame": true, "version": true,
	"config": true, "remote": true, "tag": true,
}

func isGitWriteCommand(cmd string, args []string) bool {
	if cmd != "git" {
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return !gitReadOnlySubs[a]
	}
	return false
}

func handleExec(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	toolName := strings.TrimSpace(req.Cmd)
	if !cfg.isAllowed(toolName) {
		http.Error(w, fmt.Sprintf("命令 %q 不在 HOSTEXEC_ALLOW_TOOLS 白名单中", toolName), http.StatusForbidden)
		return
	}
	binPath, err := exec.LookPath(toolName)
	if err != nil {
		http.Error(w, fmt.Sprintf("宿主机未找到命令 %q: %v", toolName, err), http.StatusBadRequest)
		return
	}
	hostCwd, err := mapGuestToHostCwd(req.Cwd, cfg.Mounts, cfg.WorkspaceHost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// Serialize git write operations across concurrent Claude sessions to prevent .git/index.lock collisions.
	if isGitWriteCommand(toolName, req.Args) {
		cfg.gitWriteMu.Lock()
		defer cfg.gitWriteMu.Unlock()
	}

	guestCwdDisplay := req.Cwd
	if strings.TrimSpace(guestCwdDisplay) == "" {
		guestCwdDisplay = cfg.WorkspaceGuest
	}
	started := time.Now()
	runID := cfg.Tracker.start(toolName, req.Args, guestCwdDisplay)

	args := rewriteArgsGuestToHost(req.Args, cfg.Mounts)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = hostCwd
	cmd.Env = mergeSafeEnv(os.Environ(), req.Env, cfg.Mounts)
	configureCmdGroup(cmd)

	if req.StdinB64 != "" {
		if decoded, dErr := base64.StdEncoding.DecodeString(req.StdinB64); dErr == nil {
			rewritten := rewriteGuestToHost(string(decoded), cfg.Mounts)
			cmd.Stdin = bytes.NewReader([]byte(rewritten))
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cfg.Tracker.finish(runID, 1, time.Since(started))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cfg.Tracker.finish(runID, 1, time.Since(started))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		cfg.Tracker.finish(runID, 1, time.Since(started))
		http.Error(w, "启动宿主机命令失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Watch client disconnect / timeout and gracefully terminate entire process group (SIGTERM -> 2s -> SIGKILL).
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			terminateCmdGroup(cmd)
		case <-doneCh:
		}
	}()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	var writeMu sync.Mutex
	sendEvt := func(ev streamEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, _ := json.Marshal(ev)
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	pump := func(streamName string, rc io.Reader) {
		reader := bufio.NewReaderSize(rc, 16*1024)
		buf := make([]byte, 4096)
		for {
			n, rErr := reader.Read(buf)
			if n > 0 {
				chunk := scrubHostToGuest(string(buf[:n]), cfg.Mounts)
				sendEvt(streamEvent{Stream: streamName, Data: chunk})
			}
			if rErr != nil {
				return
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump("stdout", stdout) }()
	go func() { defer wg.Done(); pump("stderr", stderr) }()
	wg.Wait()

	code := 0
	if waitErr := cmd.Wait(); waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 1
			sendEvt(streamEvent{Error: waitErr.Error()})
		}
	}
	close(doneCh)
	cfg.Tracker.finish(runID, code, time.Since(started))
	sendEvt(streamEvent{ExitCode: &code})
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
