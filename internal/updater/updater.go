package updater

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const AppVersion = "0.2.0"

type Component struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	CanLiveUpdate   bool   `json:"can_live_update"`
	Status          string `json:"status"` // "up_to_date" | "update_available" | "unknown"
	Note            string `json:"note,omitempty"`
}

type Report struct {
	CheckedAt     time.Time   `json:"checked_at"`
	HasUpdates    bool        `json:"has_updates"`
	UpdateCount   int         `json:"update_count"`
	Updating      bool        `json:"updating"`
	LastUpdateLog string      `json:"last_update_log,omitempty"`
	Components    []Component `json:"components"`
}

type Config struct {
	ProxyURL           string
	ClaudeBin          string
	DesktopVersionFile string
	GetMihomoVersion   func(ctx context.Context) string
}

type Checker struct {
	mu       sync.RWMutex
	cfg      Config
	report   Report
	updating bool
	lastLog  string
}

func NewChecker(cfg Config) *Checker {
	if cfg.ClaudeBin == "" {
		cfg.ClaudeBin = "claude"
	}
	return &Checker{cfg: cfg}
}

func (c *Checker) Report() Report {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r := c.report
	r.Updating = c.updating
	r.LastUpdateLog = c.lastLog
	return r
}

func (c *Checker) proxyClient() (*http.Client, error) {
	u, err := url.Parse(strings.TrimSpace(c.cfg.ProxyURL))
	if err != nil || u.Host == "" {
		return nil, errors.New("代理未配置")
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(u),
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 8 * time.Second,
		},
		Timeout: 12 * time.Second,
	}, nil
}

// CheckAll inspects installed versions and queries upstream registries strictly through the isolated proxy.
func (c *Checker) CheckAll(ctx context.Context) Report {
	client, clientErr := c.proxyClient()

	var wg sync.WaitGroup
	comps := make([]Component, 4)

	// 0: Claude Code CLI
	wg.Add(1)
	go func() {
		defer wg.Done()
		comps[0] = c.checkClaudeCode(ctx, client, clientErr)
	}()

	// 1: Claude Desktop
	wg.Add(1)
	go func() {
		defer wg.Done()
		comps[1] = c.checkClaudeDesktop(ctx, client, clientErr)
	}()

	// 2: Mihomo
	wg.Add(1)
	go func() {
		defer wg.Done()
		comps[2] = c.checkMihomo(ctx, client, clientErr)
	}()

	// 3: edualc core
	comps[3] = Component{
		ID:      "edualc",
		Name:    "edualc 控制台与隔离网关",
		Current: AppVersion,
		Latest:  AppVersion,
		Status:  "up_to_date",
		Note:    "本地自托管核心版本",
	}

	wg.Wait()

	updates := 0
	for _, comp := range comps {
		if comp.UpdateAvailable {
			updates++
		}
	}

	c.mu.Lock()
	c.report = Report{
		CheckedAt:     time.Now().UTC(),
		HasUpdates:    updates > 0,
		UpdateCount:   updates,
		Updating:      c.updating,
		LastUpdateLog: c.lastLog,
		Components:    comps,
	}
	out := c.report
	c.mu.Unlock()
	return out
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

func extractSemver(raw string) string {
	return semverRe.FindString(strings.TrimSpace(raw))
}

func (c *Checker) checkClaudeCode(ctx context.Context, client *http.Client, clientErr error) Component {
	comp := Component{
		ID:            "claude_code",
		Name:          "Claude Code CLI",
		CanLiveUpdate: true,
		Status:        "unknown",
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := exec.CommandContext(cmdCtx, c.cfg.ClaudeBin, "--version").CombinedOutput()
	cancel()
	if err == nil {
		if v := extractSemver(string(out)); v != "" {
			comp.Current = v
		} else {
			comp.Current = strings.TrimSpace(string(out))
		}
	} else {
		comp.Current = "未知"
	}

	if clientErr != nil || client == nil {
		comp.Note = "代理未就绪，暂未查询最新版本"
		return comp
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.npmjs.org/@anthropic-ai/claude-code/latest", nil)
	req.Header.Set("User-Agent", "edualc-updater/1.0")
	resp, err := client.Do(req)
	if err != nil {
		comp.Note = "查询 npm 最新版失败：" + err.Error()
		return comp
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		comp.Note = fmt.Sprintf("npm 返回 HTTP %d", resp.StatusCode)
		return comp
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&pkg) != nil || pkg.Version == "" {
		comp.Note = "解析 npm 响应失败"
		return comp
	}
	comp.Latest = strings.TrimSpace(pkg.Version)
	if comp.Current != "" && comp.Current != "未知" && comp.Current != comp.Latest {
		comp.UpdateAvailable = true
		comp.Status = "update_available"
		comp.Note = "支持容器内一键在线升级（无需重建镜像）"
	} else if comp.Current == comp.Latest {
		comp.Status = "up_to_date"
		comp.Note = "已是官方最新版本"
	}
	return comp
}

func (c *Checker) checkClaudeDesktop(ctx context.Context, client *http.Client, clientErr error) Component {
	comp := Component{
		ID:     "claude_desktop",
		Name:   "Claude Desktop (官方 Linux 版)",
		Status: "unknown",
	}
	if c.cfg.DesktopVersionFile != "" {
		if b, err := os.ReadFile(c.cfg.DesktopVersionFile); err == nil {
			var info struct {
				ClaudeDesktop string `json:"claude_desktop"`
				Chromium      string `json:"chromium"`
			}
			if json.Unmarshal(b, &info) == nil && info.ClaudeDesktop != "" {
				comp.Current = strings.TrimSpace(info.ClaudeDesktop)
			}
		}
	}
	if comp.Current == "" {
		comp.Current = "读取中"
	}

	if clientErr != nil || client == nil {
		comp.Note = "代理未就绪，暂未查询官方 apt 源"
		return comp
	}
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	aptURL := fmt.Sprintf("https://downloads.claude.ai/claude-desktop/apt/stable/dists/stable/main/binary-%s/Packages", arch)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, aptURL, nil)
	req.Header.Set("User-Agent", "edualc-updater/1.0")
	resp, err := client.Do(req)
	if err != nil {
		comp.Note = "查询 Anthropic 官方 apt 源失败：" + err.Error()
		return comp
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		comp.Note = fmt.Sprintf("官方 apt 源返回 HTTP %d", resp.StatusCode)
		return comp
	}
	latest := parseAptPackageVersion(io.LimitReader(resp.Body, 512<<10), "claude-desktop")
	if latest == "" {
		comp.Note = "未能从 apt Packages 索引解析版本"
		return comp
	}
	comp.Latest = latest
	if comp.Current != "" && comp.Current != "读取中" && comp.Current != comp.Latest {
		comp.UpdateAvailable = true
		comp.Status = "update_available"
		comp.Note = "发现官方新版，可运行重建命令更新 desktop 镜像"
	} else if comp.Current == comp.Latest {
		comp.Status = "up_to_date"
		comp.Note = "已是官方 apt 源最新版本"
	}
	return comp
}

func parseAptPackageVersion(r io.Reader, pkgName string) string {
	scanner := bufio.NewScanner(r)
	inPkg := false
	latest := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			inPkg = false
			continue
		}
		if strings.HasPrefix(line, "Package:") {
			inPkg = strings.TrimSpace(strings.TrimPrefix(line, "Package:")) == pkgName
			continue
		}
		if inPkg && strings.HasPrefix(line, "Version:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
			if v != "" {
				latest = v
			}
		}
	}
	return latest
}

func (c *Checker) checkMihomo(ctx context.Context, client *http.Client, clientErr error) Component {
	comp := Component{
		ID:     "mihomo",
		Name:   "Mihomo 代理内核",
		Status: "unknown",
	}
	if c.cfg.GetMihomoVersion != nil {
		if v := extractSemver(c.cfg.GetMihomoVersion(ctx)); v != "" {
			comp.Current = "v" + v
		}
	}
	if comp.Current == "" {
		comp.Current = "运行中"
	}
	if clientErr != nil || client == nil {
		comp.Note = "代理未就绪"
		return comp
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/MetaCubeX/mihomo/releases/latest", nil)
	req.Header.Set("User-Agent", "edualc-updater/1.0")
	resp, err := client.Do(req)
	if err != nil {
		comp.Note = "查询 GitHub Release 失败：" + err.Error()
		return comp
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		comp.Note = fmt.Sprintf("GitHub API 返回 HTTP %d", resp.StatusCode)
		return comp
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&rel) != nil || rel.TagName == "" {
		comp.Note = "解析 Release 失败"
		return comp
	}
	comp.Latest = strings.TrimSpace(rel.TagName)
	curClean := strings.TrimPrefix(comp.Current, "v")
	latClean := strings.TrimPrefix(comp.Latest, "v")
	if extractSemver(curClean) != "" && curClean != latClean {
		comp.UpdateAvailable = true
		comp.Status = "update_available"
		comp.Note = "可重建 gateway 镜像升级内核"
	} else if curClean == latClean {
		comp.Status = "up_to_date"
		comp.Note = "已是官方最新 Release"
	}
	return comp
}

// UpdateClaudeCode upgrades @anthropic-ai/claude-code in-place inside the isolated relay container via proxy.
func (c *Checker) UpdateClaudeCode(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.updating {
		c.mu.Unlock()
		return "", errors.New("已有升级任务正在运行，请稍候")
	}
	c.updating = true
	c.lastLog = "正在经隔离代理升级 @anthropic-ai/claude-code@latest ..."
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.updating = false
		c.mu.Unlock()
	}()

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	npmBin := "/usr/local/bin/npm"
	if _, statErr := os.Stat(npmBin); statErr != nil {
		npmBin = "npm"
	}
	cmd := exec.CommandContext(cmdCtx, npmBin, "install", "-g", "@anthropic-ai/claude-code@latest")
	env := os.Environ()
	if p := strings.TrimSpace(c.cfg.ProxyURL); p != "" {
		env = append(env, "HTTP_PROXY="+p, "HTTPS_PROXY="+p, "http_proxy="+p, "https_proxy="+p)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	logText := strings.TrimSpace(string(out))
	if err != nil {
		c.mu.Lock()
		c.lastLog = "升级失败: " + err.Error() + "\n" + logText
		c.mu.Unlock()
		return logText, fmt.Errorf("npm 升级失败: %w", err)
	}
	c.mu.Lock()
	c.lastLog = "升级成功！\n" + logText
	c.mu.Unlock()
	// Re-check versions immediately after upgrade.
	c.CheckAll(ctx)
	return logText, nil
}
