package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type hostAutoPolicy struct {
	AutoProxy   bool `json:"auto_proxy"`
	AutoInstall bool `json:"auto_install"`
}

type installEvent struct {
	TS         time.Time `json:"ts"`
	Tool       string    `json:"tool"`
	Action     string    `json:"action"` // "auto_proxy" | "auto_install" | "manual_install" | "blocked" | "failed"
	Method     string    `json:"method,omitempty"`
	Success    bool      `json:"success"`
	DurationMs int64     `json:"duration_ms"`
	Message    string    `json:"message"`
}

type ensureResult struct {
	Ready     bool   `json:"ready"`
	Tool      string `json:"tool"`
	Proxied   bool   `json:"proxied"`
	Installed bool   `json:"installed"`
	HostPath  string `json:"host_path,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type autoToolManager struct {
	mu             sync.Mutex
	installMu      sync.Mutex
	policyFile     string
	shimsDir       string
	hostVenvDir    string
	hostVenvReady  bool
	hostVenvPython string
	policy         hostAutoPolicy
	failedCache    map[string]time.Time
	events         []installEvent
}

var validToolNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,40}$`)

// Never auto-proxy or auto-install security-sensitive network/browser/system commands.
var blockedAutoTools = map[string]bool{
	// Anti-leak network & browser commands MUST stay inside isolated container
	"curl": true, "wget": true, "ssh": true, "scp": true, "sftp": true, "rsync": true,
	"dig": true, "nslookup": true, "host": true, "ping": true, "traceroute": true,
	"nc": true, "netcat": true, "socat": true, "telnet": true, "ftp": true,
	"chromium": true, "chrome": true, "google-chrome": true, "firefox": true,
	"safari": true, "open": true, "xdg-open": true,
	"claude": true, "claude-desktop": true, "claude-code": true,
	// Dangerous host privilege / destructive commands
	"sudo": true, "su": true, "doas": true, "rm": true, "rmdir": true, "dd": true,
	"mkfs": true, "fdisk": true, "diskutil": true, "mount": true, "umount": true,
	"shutdown": true, "reboot": true, "halt": true, "poweroff": true,
	"launchctl": true, "systemctl": true, "service": true, "kill": true, "killall": true,
	"pkill": true, "passwd": true, "useradd": true, "userdel": true, "usermod": true,
	"chown": true, "chmod": true, "chroot": true, "iptables": true, "pfctl": true,
	"sysctl": true, "csrutil": true, "nvram": true, "osascript": true,
}

// Fast local POSIX coreutils that should stay inside the container unless explicitly added.
var containerCoreutils = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ls": true, "ll": true,
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true,
	"sed": true, "awk": true, "find": true, "sort": true, "uniq": true, "wc": true,
	"cut": true, "tr": true, "xargs": true, "tee": true, "cp": true, "mv": true,
	"mkdir": true, "touch": true, "ln": true, "stat": true, "file": true, "diff": true,
	"cmp": true, "ps": true, "env": true, "which": true, "pwd": true, "echo": true,
	"printf": true, "date": true, "sleep": true, "timeout": true, "uname": true,
	"df": true, "du": true, "free": true, "nproc": true, "true": true, "false": true,
	"test": true, "yes": true, "seq": true, "od": true, "strings": true, "tar": true,
	"gzip": true, "gunzip": true, "zip": true, "unzip": true, "jq": true, "base64": true,
	"md5sum": true, "sha256sum": true, "readlink": true, "realpath": true, "basename": true,
	"dirname": true, "id": true, "whoami": true, "hostname": true, "uptime": true,
	"clear": true, "reset": true, "less": true, "more": true, "vi": true, "vim": true,
	"nano": true, "bc": true, "expr": true, "comm": true, "join": true, "paste": true,
	"split": true, "fold": true, "fmt": true, "nl": true, "rev": true, "shuf": true,
	"mktemp": true, "install": true, "nohup": true, "time": true, "pgrep": true,
	"hostshim": true, "edualc-cmd-hook": true,
}

// Well-known CLI -> installer mapping (method: "brew" | "pip" | "npm" | "winget")
type pkgMapping struct {
	Method string
	Pkg    string
}

var knownToolPackages = map[string]pkgMapping{
	"rg":           {Method: "brew", Pkg: "ripgrep"},
	"fd":           {Method: "brew", Pkg: "fd"},
	"uv":           {Method: "brew", Pkg: "uv"},
	"gh":           {Method: "brew", Pkg: "gh"},
	"ffmpeg":       {Method: "brew", Pkg: "ffmpeg"},
	"ffprobe":      {Method: "brew", Pkg: "ffmpeg"},
	"cmake":        {Method: "brew", Pkg: "cmake"},
	"ninja":        {Method: "brew", Pkg: "ninja"},
	"magick":       {Method: "brew", Pkg: "imagemagick"},
	"convert":      {Method: "brew", Pkg: "imagemagick"},
	"sqlite3":      {Method: "brew", Pkg: "sqlite"},
	"duckdb":       {Method: "brew", Pkg: "duckdb"},
	"dot":          {Method: "brew", Pkg: "graphviz"},
	"protoc":       {Method: "brew", Pkg: "protobuf"},
	"tree":         {Method: "brew", Pkg: "tree"},
	"bat":          {Method: "brew", Pkg: "bat"},
	"fzf":          {Method: "brew", Pkg: "fzf"},
	"yq":           {Method: "brew", Pkg: "yq"},
	"pandoc":       {Method: "brew", Pkg: "pandoc"},
	"tokei":        {Method: "brew", Pkg: "tokei"},
	"hyperfine":    {Method: "brew", Pkg: "hyperfine"},
	"wasm-bindgen": {Method: "brew", Pkg: "wasm-bindgen"},
	"wasm-opt":     {Method: "brew", Pkg: "binaryen"},
	"playwright":   {Method: "npm", Pkg: "playwright"},
	"tsc":          {Method: "npm", Pkg: "typescript"},
	"tsx":          {Method: "npm", Pkg: "tsx"},
	"eslint":       {Method: "npm", Pkg: "eslint"},
	"prettier":     {Method: "npm", Pkg: "prettier"},
	"vite":         {Method: "npm", Pkg: "vite"},
	"pytest":       {Method: "pip", Pkg: "pytest"},
	"ruff":         {Method: "pip", Pkg: "ruff"},
	"black":        {Method: "pip", Pkg: "black"},
	"mypy":         {Method: "pip", Pkg: "mypy"},
	"ipython":      {Method: "pip", Pkg: "ipython"},
	"jupyter":      {Method: "pip", Pkg: "jupyter"},
}

func newAutoToolManager() *autoToolManager {
	shimsAbs, _ := filepath.Abs(envOr("HOSTEXEC_SHIMS_DIR", "data/runtime/shims"))
	venvAbs, _ := filepath.Abs(envOr("HOSTEXEC_VENV_DIR", "data/host-venv"))
	m := &autoToolManager{
		policyFile:  envOr("HOSTEXEC_POLICY_FILE", "data/runtime/hostexec_policy.json"),
		shimsDir:    shimsAbs,
		hostVenvDir: venvAbs,
		policy: hostAutoPolicy{
			AutoProxy:   true,
			AutoInstall: true,
		},
		failedCache: make(map[string]time.Time),
	}
	m.loadPolicy()
	m.initHostVenvSyncOrAsync()
	return m
}

func (m *autoToolManager) loadPolicy() {
	b, err := os.ReadFile(m.policyFile)
	if err != nil {
		return
	}
	var p hostAutoPolicy
	if json.Unmarshal(b, &p) == nil {
		m.mu.Lock()
		m.policy = p
		m.mu.Unlock()
	}
}

func (m *autoToolManager) savePolicyLocked() {
	_ = os.MkdirAll(filepath.Dir(m.policyFile), 0o755)
	if b, err := json.MarshalIndent(m.policy, "", "  "); err == nil {
		_ = os.WriteFile(m.policyFile, b, 0o644)
	}
}

func (m *autoToolManager) getPolicy() hostAutoPolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

func (m *autoToolManager) updatePolicy(p hostAutoPolicy) hostAutoPolicy {
	m.mu.Lock()
	m.policy = p
	m.savePolicyLocked()
	out := m.policy
	m.mu.Unlock()
	return out
}

func (m *autoToolManager) recordEvent(ev installEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	m.events = append([]installEvent{ev}, m.events...)
	if len(m.events) > 25 {
		m.events = m.events[:25]
	}
}

func (m *autoToolManager) snapshot() (hostAutoPolicy, bool, string, []installEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := make([]installEvent, len(m.events))
	copy(evs, m.events)
	return m.policy, m.hostVenvReady, m.hostVenvPython, evs
}

func (m *autoToolManager) venvBinDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(m.hostVenvDir, "Scripts")
	}
	return filepath.Join(m.hostVenvDir, "bin")
}

// initHostVenvSyncOrAsync activates an existing host-venv immediately (<1ms),
// or creates it in the background so hostexec HTTP startup is never delayed.
func (m *autoToolManager) initHostVenvSyncOrAsync() {
	binDir := m.venvBinDir()
	pyName := "python3"
	if runtime.GOOS == "windows" {
		pyName = "python.exe"
	}
	venvPy := filepath.Join(binDir, pyName)
	if _, err := os.Stat(venvPy); err == nil {
		m.activateHostVenv(binDir, venvPy)
		return
	}
	go func() {
		basePy := findBestHostPython()
		if basePy == "" {
			log.Printf("[hostexec] 未检测到宿主机 Python，跳过初始化专属 venv")
			return
		}
		_ = os.MkdirAll(filepath.Dir(m.hostVenvDir), 0o755)
		log.Printf("[hostexec] 后台初始化宿主机专属 Python 环境: %s (base=%s)", m.hostVenvDir, basePy)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, basePy, "-m", "venv", m.hostVenvDir)
		if out, vErr := cmd.CombinedOutput(); vErr != nil {
			log.Printf("[hostexec] 初始化宿主机 venv 失败: %v (%s)", vErr, strings.TrimSpace(string(out)))
			return
		}
		m.activateHostVenv(binDir, venvPy)
		for _, tool := range []string{"python3", "python", "pip", "pip3"} {
			if _, err := exec.LookPath(tool); err == nil {
				_ = m.ensureShimSymlink(tool)
			}
		}
	}()
}

func (m *autoToolManager) activateHostVenv(binDir, venvPy string) {
	curPath := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	if !strings.HasPrefix(curPath, binDir+sep) {
		_ = os.Setenv("PATH", binDir+sep+curPath)
	}
	verOut, _ := exec.Command(venvPy, "--version").CombinedOutput()
	m.mu.Lock()
	m.hostVenvReady = true
	m.hostVenvPython = strings.TrimSpace(string(verOut))
	m.mu.Unlock()
	log.Printf("[hostexec] 宿主机专属 Python 环境就绪: %s (%s)", m.hostVenvDir, strings.TrimSpace(string(verOut)))
}

func findBestHostPython() string {
	// Prefer 3.12 -> 3.13 -> 3.11 -> python3 -> python for best prebuilt binary wheel support (scipy/numpy/torch).
	candidates := []string{
		"/opt/homebrew/bin/python3.12",
		"/usr/local/bin/python3.12",
		"python3.12",
		"/opt/homebrew/bin/python3.13",
		"/usr/local/bin/python3.13",
		"python3.13",
		"python3.11",
		"python3",
		"python",
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil && p != "" {
			// Skip shim or broken symlink inside our own venv
			if strings.Contains(p, "host-venv") {
				continue
			}
			return p
		}
	}
	return ""
}

func (m *autoToolManager) ensureShimSymlink(name string) error {
	if err := os.MkdirAll(m.shimsDir, 0o755); err != nil {
		return err
	}
	linkPath := filepath.Join(m.shimsDir, name)
	// On Windows hosts, symlinks require Administrator/Developer Mode and do not execute cleanly
	// across NTFS -> Linux Docker Desktop bind mounts; emit a tiny LF POSIX wrapper script instead.
	if runtime.GOOS == "windows" {
		wrapper := fmt.Sprintf("#!/bin/sh\nexec /usr/local/bin/hostshim %q \"$@\"\n", name)
		if existing, err := os.ReadFile(linkPath); err == nil && string(existing) == wrapper {
			return nil
		}
		_ = os.Remove(linkPath)
		return os.WriteFile(linkPath, []byte(wrapper), 0o755)
	}
	if target, err := os.Readlink(linkPath); err == nil && target == "/usr/local/bin/hostshim" {
		return nil
	}
	_ = os.Remove(linkPath)
	return os.Symlink("/usr/local/bin/hostshim", linkPath)
}

func (m *autoToolManager) syncAllShims(cfg *hostConfig) {
	for _, name := range cfg.toolNames() {
		if _, err := exec.LookPath(name); err == nil {
			_ = m.ensureShimSymlink(name)
		}
	}
}

func isUnsafeAutoTool(name string) (bool, string) {
	if !validToolNameRe.MatchString(name) {
		return true, "无效命令名称"
	}
	low := strings.ToLower(name)
	if blockedAutoTools[low] {
		return true, "出于安全与防泄露策略，该命令禁止透传至宿主机"
	}
	return false, ""
}

func containsForbiddenAnthropicTarget(args []string) bool {
	for _, a := range args {
		low := strings.ToLower(a)
		if strings.Contains(low, "api.anthropic.com") ||
			strings.Contains(low, "claude.ai") ||
			strings.Contains(low, "console.anthropic.com") ||
			strings.Contains(low, "@anthropic-ai/claude-code") {
			return true
		}
	}
	return false
}

// ensureTool checks if `name` can be proxied to host; if missing and allowed, auto-installs it on host.
func (m *autoToolManager) ensureTool(ctx context.Context, cfg *hostConfig, name string, missingInGuest bool, forceInstall bool) ensureResult {
	name = strings.TrimSpace(name)
	if blocked, reason := isUnsafeAutoTool(name); blocked {
		return ensureResult{Tool: name, Reason: reason}
	}

	// Already allowed and present on host -> make sure shim exists and return fast.
	if cfg.isAllowed(name) {
		if p, err := exec.LookPath(name); err == nil && p != "" {
			_ = m.ensureShimSymlink(name)
			return ensureResult{Ready: true, Tool: name, Proxied: true, HostPath: p}
		}
	}

	// Unless forced (UI manual install), don't hijack standard POSIX coreutils that exist in container.
	if !forceInstall && !missingInGuest && containerCoreutils[name] {
		return ensureResult{Tool: name, Reason: "容器内置基础命令，无需透传"}
	}

	pol := m.getPolicy()
	if !forceInstall && !pol.AutoProxy && !pol.AutoInstall {
		return ensureResult{Tool: name, Reason: "自动接管与自动安装已关闭"}
	}

	// Case 1: Host ALREADY has this command -> auto-proxy immediately!
	if p, err := exec.LookPath(name); err == nil && p != "" {
		if forceInstall || pol.AutoProxy {
			started := time.Now()
			if err := cfg.addTool(name); err == nil {
				_ = m.ensureShimSymlink(name)
				m.recordEvent(installEvent{
					Tool:       name,
					Action:     "auto_proxy",
					Method:     "host_path",
					Success:    true,
					DurationMs: time.Since(started).Milliseconds(),
					Message:    "宿主机已有该命令，已自动加入白名单并挂接代理: " + p,
				})
				log.Printf("[hostexec] auto-proxied existing host tool %q (%s)", name, p)
				return ensureResult{Ready: true, Tool: name, Proxied: true, HostPath: p}
			}
		}
		return ensureResult{Tool: name, Reason: "宿主机已有该命令，但未开启自动接管"}
	}

	// Case 2: Host does NOT have this command -> auto-install if enabled or forced!
	if !forceInstall && (!pol.AutoInstall || !missingInGuest) {
		return ensureResult{Tool: name, Reason: "宿主机未安装该命令"}
	}

	// Check negative cache to avoid repeatedly trying bogus/typo command names.
	if !forceInstall {
		m.mu.Lock()
		lastFail, failed := m.failedCache[name]
		m.mu.Unlock()
		if failed && time.Since(lastFail) < 30*time.Minute {
			return ensureResult{Tool: name, Reason: "近期自动安装未匹配到可用软件包（已缓存跳过）"}
		}
	}

	m.installMu.Lock()
	defer m.installMu.Unlock()

	// Double-check after acquiring lock in case another goroutine just installed it.
	if p, err := exec.LookPath(name); err == nil && p != "" {
		_ = cfg.addTool(name)
		_ = m.ensureShimSymlink(name)
		return ensureResult{Ready: true, Tool: name, Proxied: true, HostPath: p}
	}

	started := time.Now()
	method, msg, err := m.installOnHost(ctx, name)
	dur := time.Since(started)
	action := "auto_install"
	if forceInstall {
		action = "manual_install"
	}
	if err != nil {
		m.mu.Lock()
		m.failedCache[name] = time.Now()
		m.mu.Unlock()
		m.recordEvent(installEvent{
			Tool:       name,
			Action:     "failed",
			Method:     method,
			Success:    false,
			DurationMs: dur.Milliseconds(),
			Message:    err.Error(),
		})
		return ensureResult{Tool: name, Reason: err.Error()}
	}

	// Re-check PATH after install
	p, err := exec.LookPath(name)
	if err != nil || p == "" {
		errMsg := fmt.Sprintf("安装完成但 PATH 中仍未找到 %q (%s)", name, msg)
		m.recordEvent(installEvent{
			Tool:       name,
			Action:     "failed",
			Method:     method,
			Success:    false,
			DurationMs: dur.Milliseconds(),
			Message:    errMsg,
		})
		return ensureResult{Tool: name, Reason: errMsg}
	}

	_ = cfg.addTool(name)
	_ = m.ensureShimSymlink(name)
	m.recordEvent(installEvent{
		Tool:       name,
		Action:     action,
		Method:     method,
		Success:    true,
		DurationMs: dur.Milliseconds(),
		Message:    fmt.Sprintf("宿主机已通过 %s 自动安装并挂接代理: %s", method, p),
	})
	log.Printf("[hostexec] installed & proxied host tool %q via %s (%s)", name, method, p)
	return ensureResult{Ready: true, Tool: name, Proxied: true, Installed: true, HostPath: p}
}

func (m *autoToolManager) installOnHost(ctx context.Context, name string) (string, string, error) {
	installCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	// Check explicit known mapping first
	if mapping, ok := knownToolPackages[name]; ok {
		switch mapping.Method {
		case "pip":
			return m.installViaHostVenvPip(installCtx, mapping.Pkg)
		case "npm":
			return m.installViaHostNpm(installCtx, mapping.Pkg)
		case "brew":
			if runtime.GOOS == "darwin" || hasBinary("brew") {
				return m.installViaBrew(installCtx, mapping.Pkg, true)
			}
			if runtime.GOOS == "windows" {
				return m.installViaWinget(installCtx, mapping.Pkg)
			}
		}
	}

	// Generic OS package manager fallback
	switch runtime.GOOS {
	case "darwin":
		if hasBinary("brew") {
			return m.installViaBrew(installCtx, name, false)
		}
		return "brew", "", errors.New("宿主机未安装 Homebrew")
	case "windows":
		if hasBinary("winget") {
			return m.installViaWinget(installCtx, name)
		}
		return "winget", "", errors.New("宿主机未安装 winget")
	default:
		if hasBinary("brew") {
			return m.installViaBrew(installCtx, name, false)
		}
		return "pkg", "", fmt.Errorf("宿主机暂不支持自动安装 %q，请手动安装后再加入代理", name)
	}
}

func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (m *autoToolManager) installViaHostVenvPip(ctx context.Context, pkg string) (string, string, error) {
	pipPath := filepath.Join(m.venvBinDir(), "pip")
	if runtime.GOOS == "windows" {
		pipPath += ".exe"
	}
	if _, err := os.Stat(pipPath); err != nil {
		return "pip", "", errors.New("宿主机专属 Python venv 尚未就绪")
	}
	cmd := exec.CommandContext(ctx, pipPath, "install", "--disable-pip-version-check", pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "pip", "", fmt.Errorf("pip install %s 失败: %s", pkg, tailLines(string(out), 4))
	}
	return "pip(host-venv)", pkg, nil
}

func (m *autoToolManager) installViaHostNpm(ctx context.Context, pkg string) (string, string, error) {
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return "npm", "", errors.New("宿主机未找到 npm")
	}
	cmd := exec.CommandContext(ctx, npmPath, "install", "-g", pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "npm", "", fmt.Errorf("npm install -g %s 失败: %s", pkg, tailLines(string(out), 4))
	}
	return "npm", pkg, nil
}

func (m *autoToolManager) installViaBrew(ctx context.Context, pkg string, skipProbe bool) (string, string, error) {
	brewPath, err := exec.LookPath("brew")
	if err != nil {
		return "brew", "", errors.New("宿主机未安装 brew")
	}
	env := append(os.Environ(),
		"NONINTERACTIVE=1",
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_INSTALL_CLEANUP=1",
	)
	if !skipProbe {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		probe := exec.CommandContext(probeCtx, brewPath, "info", "--formula", "--json=v2", pkg)
		probe.Env = env
		if err := probe.Run(); err != nil {
			return "brew", "", fmt.Errorf("Homebrew 未找到名为 %q 的公式", pkg)
		}
	}
	cmd := exec.CommandContext(ctx, brewPath, "install", pkg)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "brew", "", fmt.Errorf("brew install %s 失败: %s", pkg, tailLines(string(out), 5))
	}
	return "brew", pkg, nil
}

func (m *autoToolManager) installViaWinget(ctx context.Context, pkg string) (string, string, error) {
	wingetPath, err := exec.LookPath("winget")
	if err != nil {
		return "winget", "", errors.New("宿主机未安装 winget")
	}
	cmd := exec.CommandContext(ctx, wingetPath, "install", "--exact", "--silent", "--accept-source-agreements", "--accept-package-agreements", pkg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "winget", "", fmt.Errorf("winget install %s 失败: %s", pkg, tailLines(string(out), 5))
	}
	return "winget", pkg, nil
}

func tailLines(s string, maxLines int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}

func handleEnsureTool(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Name           string `json:"name"`
		MissingInGuest bool   `json:"missing_in_guest"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res := cfg.AutoMgr.ensureTool(r.Context(), cfg, req.Name, req.MissingInGuest, false)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(res)
}

func handleInstallTool(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
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
	res := cfg.AutoMgr.ensureTool(r.Context(), cfg, req.Name, true, true)
	if !res.Ready {
		http.Error(w, res.Reason, http.StatusBadRequest)
		return
	}
	handleStatus(w, r, cfg)
}

func handleAutoPolicy(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var p hostAutoPolicy
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.AutoMgr.updatePolicy(p)
	handleStatus(w, r, cfg)
}
