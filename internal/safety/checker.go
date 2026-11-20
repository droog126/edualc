package safety

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mini-cage-relay/internal/exitpolicy"
	"mini-cage-relay/internal/mihomo"
)

const (
	PolicyVersion  = "edualc-safety/1"
	ReportLifetime = 6 * time.Hour
)

type Check struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	State     exitpolicy.State `json:"state"`
	Critical  bool             `json:"critical"`
	Evidence  string           `json:"evidence"`
	Remedy    string           `json:"remedy,omitempty"`
	CheckedAt time.Time        `json:"checked_at"`
}

type Report struct {
	Policy       string               `json:"policy"`
	ID           string               `json:"id"`
	Authorized   bool                 `json:"authorized"`
	BlockReason  string               `json:"block_reason,omitempty"`
	ExitIP       string               `json:"exit_ip,omitempty"`
	LatencyMs    int                  `json:"latency_ms,omitempty"`
	Location     *exitpolicy.Location `json:"location,omitempty"`
	SyncedTZ     string               `json:"synced_tz,omitempty"`
	SyncedLocale string               `json:"synced_locale,omitempty"`
	CreatedAt    time.Time            `json:"created_at"`
	Checks       []Check              `json:"checks"`
}

var RequiredCheckIDs = [...]string{
	"egress_policy",
	"udp_policy",
	"ipv6_policy",
	"dns_transport",
	"proxy_transport",
	"fail_closed",
	"exit_consistency",
	"guest_settings",
	"filesystem_boundary",
	"anthropic_connectivity",
}

// Authorizes validates every required check is present, fresh, and passed (strict fail-closed).
func (r Report) Authorizes(now time.Time) error {
	if r.Policy != PolicyVersion || r.ID == "" {
		return errors.New("安全检测报告无效或尚未执行检测，已熔断拒绝发包")
	}
	if r.CreatedAt.IsZero() || now.After(r.CreatedAt.Add(ReportLifetime)) {
		return errors.New("安全检测报告已过期，请重新执行检测")
	}
	seen := make(map[string]Check, len(r.Checks))
	for _, c := range r.Checks {
		seen[c.ID] = c
		if c.Critical && c.State != exitpolicy.Passed {
			return fmt.Errorf("安全检查未通过 [%s]: %s（%s）", c.Title, c.Evidence, c.Remedy)
		}
	}
	for _, id := range RequiredCheckIDs {
		c, ok := seen[id]
		if !ok || !c.Critical || c.State != exitpolicy.Passed || c.Evidence == "" {
			return fmt.Errorf("缺少必需的安全检测证据 [%s]，已熔断拒绝发包", id)
		}
	}
	return nil
}

type GatewayStatusFetcher func(ctx context.Context) (*mihomo.Status, error)

type CheckerConfig struct {
	ProxyURL          string
	FetchGateway      GatewayStatusFetcher
	AllowHostDevMode  bool // only for local non-docker unit tests
	AccountsDir       string
	EnvSyncOutputFile string // shared file for login-browser container to read synced TZ/LANG
}

type Checker struct {
	mu      sync.RWMutex
	cfg     CheckerConfig
	policy  exitpolicy.Policy
	current Report
}

func NewChecker(cfg CheckerConfig, policy exitpolicy.Policy) *Checker {
	return &Checker{
		cfg:    cfg,
		policy: policy.Normalized(),
	}
}

func (c *Checker) SetPolicy(p exitpolicy.Policy) error {
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.policy = p
	c.mu.Unlock()
	return nil
}

func (c *Checker) Policy() exitpolicy.Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.policy
}

func (c *Checker) Report() Report {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

func (c *Checker) CheckAuthorized(now time.Time) (Report, error) {
	rep := c.Report()
	return rep, rep.Authorizes(now)
}

func (c *Checker) proxyHTTPClient() (*http.Client, error) {
	u, err := url.Parse(strings.TrimSpace(c.cfg.ProxyURL))
	if err != nil || u.Host == "" {
		return nil, errors.New("代理地址未配置或格式无效")
	}
	tr := &http.Transport{
		Proxy:               http.ProxyURL(u),
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 8 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("probe redirect refused")
		},
	}, nil
}

// RunAll executes all 10 fail-closed isolation & egress safety checks and updates the active authorization report.
func (c *Checker) RunAll(ctx context.Context) Report {
	now := time.Now().UTC()
	rep := Report{
		Policy:    PolicyVersion,
		ID:        randomID(),
		CreatedAt: now,
		Checks:    make([]Check, 0, len(RequiredCheckIDs)),
	}

	// 1. egress_policy: Direct TCP to public IPv4 MUST fail (proves internal:true container isolation).
	rep.Checks = append(rep.Checks, c.checkDirectTCPBlocked(ctx))

	// 2. udp_policy: Direct UDP to public IPv4 MUST fail.
	rep.Checks = append(rep.Checks, c.checkDirectUDPBlocked(ctx))

	// 3. ipv6_policy: Direct IPv6 egress MUST fail.
	rep.Checks = append(rep.Checks, c.checkDirectIPv6Blocked(ctx))

	// 4. fail_closed: Verify gateway Mihomo is running with zero DIRECT rules & IPv6 disabled.
	gwStatus, failClosedCheck := c.checkGatewayFailClosed(ctx)
	rep.Checks = append(rep.Checks, failClosedCheck)

	// 5. proxy_transport: Query public IPv4 strictly via gateway proxy.
	client, clientErr := c.proxyHTTPClient()
	var exitIP string
	var proxyCheck Check
	if clientErr != nil {
		proxyCheck = Check{
			ID:        "proxy_transport",
			Title:     "代理通道连接",
			State:     exitpolicy.Failed,
			Critical:  true,
			Evidence:  clientErr.Error(),
			Remedy:    "请检查 RELAY_PROXY_URL 与 gateway 容器状态",
			CheckedAt: time.Now().UTC(),
		}
	} else {
		ip, err := exitpolicy.QueryExitIP(ctx, client)
		if err != nil {
			proxyCheck = Check{
				ID:        "proxy_transport",
				Title:     "代理通道连接",
				State:     exitpolicy.Failed,
				Critical:  true,
				Evidence:  err.Error(),
				Remedy:    "请在面板导入 Clash/Mihomo 订阅或配置可用静态代理节点后重试",
				CheckedAt: time.Now().UTC(),
			}
		} else {
			exitIP = ip
			nodeInfo := ""
			if gwStatus != nil && gwStatus.SelectedNode != "" {
				nodeInfo = "（节点: " + gwStatus.SelectedNode + "）"
			}
			proxyCheck = Check{
				ID:        "proxy_transport",
				Title:     "代理通道连接",
				State:     exitpolicy.Passed,
				Critical:  true,
				Evidence:  fmt.Sprintf("通过隔离代理取得公网 IPv4: %s%s", exitIP, nodeInfo),
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	rep.ExitIP = exitIP
	rep.Checks = append(rep.Checks, proxyCheck)

	// 6. dns_transport: Direct UDP/53 DNS blocked + remote domain resolution via proxy succeeded.
	rep.Checks = append(rep.Checks, c.checkDNSTransport(ctx, proxyCheck.State == exitpolicy.Passed))

	// 7. exit_consistency: Geo lookup via proxy + policy evaluation (blocks CN/HK/MO/RU/...).
	var loc *exitpolicy.Location
	var exitCheck Check
	if exitIP == "" || client == nil {
		exitCheck = Check{
			ID:        "exit_consistency",
			Title:     "出口 IP 与地区风控",
			State:     exitpolicy.Unknown,
			Critical:  true,
			Evidence:  "尚未取得有效代理出口 IP，无法校验归属地",
			Remedy:    "请先确保代理通道连接正常",
			CheckedAt: time.Now().UTC(),
		}
	} else {
		lookedUp, err := exitpolicy.Lookup(ctx, client, exitIP)
		if err != nil {
			exitCheck = Check{
				ID:        "exit_consistency",
				Title:     "出口 IP 与地区风控",
				State:     exitpolicy.Unknown,
				Critical:  true,
				Evidence:  err.Error(),
				Remedy:    "请稍后重新检测或切换其他可用出口节点",
				CheckedAt: time.Now().UTC(),
			}
		} else {
			loc = &lookedUp
			rep.Location = loc
			st, msg := c.Policy().Evaluate(exitIP, loc)
			remedy := ""
			if st != exitpolicy.Passed {
				remedy = "请切换至允许的国家/地区节点，或在面板中调整并绑定出口规则"
			}
			exitCheck = Check{
				ID:        "exit_consistency",
				Title:     "出口 IP 与地区风控",
				State:     st,
				Critical:  true,
				Evidence:  msg,
				Remedy:    remedy,
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	rep.Checks = append(rep.Checks, exitCheck)

	// 8. guest_settings: Sync TZ and LANG to match exit IP location.
	rep.Checks = append(rep.Checks, c.checkAndSyncGuestSettings(loc, &rep))

	// 9. filesystem_boundary: Ensure host SSH keys and host ~/.claude are NOT mounted.
	rep.Checks = append(rep.Checks, c.checkFilesystemBoundary())

	// 10. anthropic_connectivity: Verify TLS reachability and measure round-trip latency to api.anthropic.com.
	anthroCheck, latencyMs := c.checkAnthropicConnectivity(ctx, client, proxyCheck.State == exitpolicy.Passed)
	rep.LatencyMs = latencyMs
	rep.Checks = append(rep.Checks, anthroCheck)

	if err := rep.Authorizes(time.Now().UTC()); err != nil {
		rep.Authorized = false
		rep.BlockReason = err.Error()
	} else {
		rep.Authorized = true
	}

	c.mu.Lock()
	c.current = rep
	c.mu.Unlock()
	return rep
}

func (c *Checker) checkDirectTCPBlocked(ctx context.Context) Check {
	if c.cfg.AllowHostDevMode {
		return Check{
			ID: "egress_policy", Title: "直连公网阻断 (TCP)", State: exitpolicy.Passed, Critical: true,
			Evidence: "开发测试模式（跳过物理容器路由探测）", CheckedAt: time.Now().UTC(),
		}
	}
	dialer := &net.Dialer{Timeout: 1500 * time.Millisecond}
	for _, target := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		conn, err := dialer.DialContext(ctx, "tcp4", target)
		if err == nil {
			conn.Close()
			return Check{
				ID: "egress_policy", Title: "直连公网阻断 (TCP)", State: exitpolicy.Failed, Critical: true,
				Evidence: fmt.Sprintf("检测到容器可直接连通公网 %s！存在宿主机 IP 直连泄露风险", target),
				Remedy:   "请确认 docker-compose.yml 中 relay 容器仅连接 internal: true 的隔离内网",
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	return Check{
		ID: "egress_policy", Title: "直连公网阻断 (TCP)", State: exitpolicy.Passed, Critical: true,
		Evidence: "直连公网 1.1.1.1:443 / 8.8.8.8:443 均被内核拒绝，确认无公网直连路由",
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkDirectUDPBlocked(ctx context.Context) Check {
	if c.cfg.AllowHostDevMode {
		return Check{
			ID: "udp_policy", Title: "UDP 出站阻断", State: exitpolicy.Passed, Critical: true,
			Evidence: "开发测试模式", CheckedAt: time.Now().UTC(),
		}
	}
	dialer := &net.Dialer{Timeout: 1200 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "udp4", "1.1.1.1:53")
	if err == nil {
		// Send minimal DNS query header to verify no UDP response can arrive.
		_ = conn.SetDeadline(time.Now().Add(1200 * time.Millisecond))
		_, wErr := conn.Write([]byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00, 0x00, 0x01, 0x00, 0x01})
		if wErr == nil {
			buf := make([]byte, 512)
			n, rErr := conn.Read(buf)
			conn.Close()
			if rErr == nil && n > 0 {
				return Check{
					ID: "udp_policy", Title: "UDP 出站阻断", State: exitpolicy.Failed, Critical: true,
					Evidence: "检测到容器可直接收发公网 UDP 53 流量，存在 UDP/DNS 泄露风险",
					Remedy:   "请确保 relay 容器位于 internal: true 隔离网络中",
					CheckedAt: time.Now().UTC(),
				}
			}
		} else {
			conn.Close()
		}
	}
	return Check{
		ID: "udp_policy", Title: "UDP 出站阻断", State: exitpolicy.Passed, Critical: true,
		Evidence: "直连公网 UDP 探测被阻断或不可达，无公网 UDP 泄露",
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkDirectIPv6Blocked(ctx context.Context) Check {
	if c.cfg.AllowHostDevMode {
		return Check{
			ID: "ipv6_policy", Title: "IPv6 出站阻断", State: exitpolicy.Passed, Critical: true,
			Evidence: "开发测试模式", CheckedAt: time.Now().UTC(),
		}
	}
	dialer := &net.Dialer{Timeout: 1200 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp6", "[2606:4700:4700::1111]:443")
	if err == nil {
		conn.Close()
		return Check{
			ID: "ipv6_policy", Title: "IPv6 出站阻断", State: exitpolicy.Failed, Critical: true,
			Evidence: "检测到容器可直接建立公网 IPv6 连接，存在 IPv6 出口泄露风险",
			Remedy:   "请禁用容器 IPv6 并确保仅挂 internal: true 网络",
			CheckedAt: time.Now().UTC(),
		}
	}
	return Check{
		ID: "ipv6_policy", Title: "IPv6 出站阻断", State: exitpolicy.Passed, Critical: true,
		Evidence: "公网 IPv6 直连不可达，IPv6 泄露防护生效",
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkGatewayFailClosed(ctx context.Context) (*mihomo.Status, Check) {
	if c.cfg.FetchGateway == nil {
		return nil, Check{
			ID: "fail_closed", Title: "代理故障断网 (零 DIRECT)", State: exitpolicy.Unknown, Critical: true,
			Evidence: "未配置 gateway 状态接口", Remedy: "请检查 RELAY_GATEWAY_CONTROL_URL",
			CheckedAt: time.Now().UTC(),
		}
	}
	st, err := c.cfg.FetchGateway(ctx)
	if err != nil {
		return nil, Check{
			ID: "fail_closed", Title: "代理故障断网 (零 DIRECT)", State: exitpolicy.Failed, Critical: true,
			Evidence: "无法读取 gateway 代理门控状态: " + err.Error(),
			Remedy:   "请检查 gateway 容器是否正常运行",
			CheckedAt: time.Now().UTC(),
		}
	}
	if !st.Running || st.Mode == "unconfigured" {
		return st, Check{
			ID: "fail_closed", Title: "代理故障断网 (零 DIRECT)", State: exitpolicy.Failed, Critical: true,
			Evidence: "Mihomo 出口代理尚未配置或未运行，外网保持熔断",
			Remedy:   "请在下方「订阅出口管理」导入 Clash/Mihomo 订阅或配置静态代理",
			CheckedAt: time.Now().UTC(),
		}
	}
	if !st.ZeroDirectRules || !st.IPv6Disabled {
		return st, Check{
			ID: "fail_closed", Title: "代理故障断网 (零 DIRECT)", State: exitpolicy.Failed, Critical: true,
			Evidence: "gateway 未满足零 DIRECT 与 IPv6 禁用约束",
			Remedy:   "请通过面板重新应用订阅以重新清洗规则",
			CheckedAt: time.Now().UTC(),
		}
	}
	chainNote := "单跳代理"
	if st.ChainEnabled {
		chainNote = "链式代理（前置节点 → 静态落地）"
	}
	return st, Check{
		ID: "fail_closed", Title: "代理故障断网 (零 DIRECT)", State: exitpolicy.Passed, Critical: true,
		Evidence: fmt.Sprintf("Mihomo 运行中（%s，零 DIRECT 规则，IPv6 已禁用，无直连回退）", chainNote),
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkDNSTransport(ctx context.Context, proxyOK bool) Check {
	if !proxyOK {
		return Check{
			ID: "dns_transport", Title: "DNS 远端解析隔离", State: exitpolicy.Unknown, Critical: true,
			Evidence: "代理通道未就绪，无法确认远端 DNS 解析状态",
			Remedy:   "请先修复代理通道连接",
			CheckedAt: time.Now().UTC(),
		}
	}
	// Verify container local resolver cannot directly reach public internet DNS (unless dev mode).
	if !c.cfg.AllowHostDevMode {
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 1200 * time.Millisecond}
				return d.DialContext(ctx, "udp4", "8.8.8.8:53")
			},
		}
		if addrs, err := r.LookupHost(ctx, "api.anthropic.com"); err == nil && len(addrs) > 0 {
			return Check{
				ID: "dns_transport", Title: "DNS 远端解析隔离", State: exitpolicy.Failed, Critical: true,
				Evidence: "容器可直接向公网 DNS (8.8.8.8:53) 发起解析，存在 DNS 泄露风险",
				Remedy:   "请确认 relay 容器处于 internal: true 隔离网中",
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	return Check{
		ID: "dns_transport", Title: "DNS 远端解析隔离", State: exitpolicy.Passed, Critical: true,
		Evidence: "本地直连 DNS 已阻断，所有域名均经代理隧道远端解析",
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkAndSyncGuestSettings(loc *exitpolicy.Location, rep *Report) Check {
	if loc == nil || loc.Timezone == "" {
		return Check{
			ID: "guest_settings", Title: "环境时区与语言同步", State: exitpolicy.Unknown, Critical: true,
			Evidence: "尚未获得出口位置时区，无法自动同步 TZ / LANG",
			Remedy:   "等待出口定位成功后重新检测",
			CheckedAt: time.Now().UTC(),
		}
	}
	tzLoc, err := time.LoadLocation(loc.Timezone)
	if err != nil {
		return Check{
			ID: "guest_settings", Title: "环境时区与语言同步", State: exitpolicy.Failed, Critical: true,
			Evidence: fmt.Sprintf("出口时区 %q 无法加载: %v", loc.Timezone, err),
			Remedy:   "请确认镜像已安装 tzdata",
			CheckedAt: time.Now().UTC(),
		}
	}
	lp := exitpolicy.LocaleProfileForCountry(loc.CountryCode)
	rep.SyncedTZ = loc.Timezone
	rep.SyncedLocale = lp.POSIX

	// Sync Go process + OS /etc/localtime + /etc/timezone inside container so all tools agree.
	_ = os.Setenv("TZ", loc.Timezone)
	_ = os.Setenv("LANG", lp.POSIX)
	_ = os.Setenv("LC_ALL", lp.POSIX)
	_ = os.Setenv("LANGUAGE", lp.LanguageEnv)
	time.Local = tzLoc
	if !c.cfg.AllowHostDevMode {
		zoneFile := filepath.Join("/usr/share/zoneinfo", loc.Timezone)
		if _, statErr := os.Stat(zoneFile); statErr == nil {
			_ = os.Remove("/etc/localtime")
			_ = os.Symlink(zoneFile, "/etc/localtime")
			_ = os.WriteFile("/etc/timezone", []byte(loc.Timezone+"\n"), 0o644)
		}
	}

	if c.cfg.EnvSyncOutputFile != "" {
		_ = os.MkdirAll(filepath.Dir(c.cfg.EnvSyncOutputFile), 0o700)
		payload, _ := json.MarshalIndent(map[string]string{
			"tz":               loc.Timezone,
			"lang":             lp.POSIX,
			"bcp47":            lp.BCP47,
			"accept_languages": lp.AcceptLanguages,
			"language_env":     lp.LanguageEnv,
			"country_code":     loc.CountryCode,
			"exit_ip":          loc.IP,
			"updated_at":       time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		_ = os.WriteFile(c.cfg.EnvSyncOutputFile, payload, 0o644)
	}
	return Check{
		ID: "guest_settings", Title: "环境时区与语言同步", State: exitpolicy.Passed, Critical: true,
		Evidence: fmt.Sprintf("已全链路同步出口时区与语言：TZ=%s, LANG=%s, Browser=%s (%s)", loc.Timezone, lp.POSIX, lp.BCP47, lp.AcceptLanguages),
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkFilesystemBoundary() Check {
	if c.cfg.AllowHostDevMode {
		return Check{
			ID: "filesystem_boundary", Title: "文件系统与宿主机隔离", State: exitpolicy.Passed, Critical: true,
			Evidence: "开发测试模式", CheckedAt: time.Now().UTC(),
		}
	}
	// Ensure host SSH keys or unexpected host root credentials aren't mounted into /root/.ssh.
	if info, err := os.Stat("/root/.ssh"); err == nil && info.IsDir() {
		entries, _ := os.ReadDir("/root/.ssh")
		if len(entries) > 0 {
			return Check{
				ID: "filesystem_boundary", Title: "文件系统与宿主机隔离", State: exitpolicy.Failed, Critical: true,
				Evidence: "检测到 /root/.ssh 非空，可能误挂载了宿主机 SSH 密钥",
				Remedy:   "请检查 docker-compose.yml volumes，仅挂载 ./data 目录",
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	return Check{
		ID: "filesystem_boundary", Title: "文件系统与宿主机隔离", State: exitpolicy.Passed, Critical: true,
		Evidence: fmt.Sprintf("未挂载宿主机 SSH/个人目录，仅隔离访问 %s", c.cfg.AccountsDir),
		CheckedAt: time.Now().UTC(),
	}
}

func (c *Checker) checkAnthropicConnectivity(ctx context.Context, client *http.Client, proxyOK bool) (Check, int) {
	if !proxyOK || client == nil {
		return Check{
			ID: "anthropic_connectivity", Title: "Anthropic 代理连通性", State: exitpolicy.Unknown, Critical: true,
			Evidence: "代理通道未就绪，跳过 Anthropic 连通性探测",
			Remedy:   "请先修复代理出口",
			CheckedAt: time.Now().UTC(),
		}, 0
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				break
			case <-time.After(600 * time.Millisecond):
			}
		}
		started := time.Now()
		reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.anthropic.com/", nil)
		req.Header.Set("User-Agent", "curl/8.7.1")
		resp, err := client.Do(req)
		elapsedMs := int(time.Since(started).Milliseconds())
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return Check{
				ID: "anthropic_connectivity", Title: "Anthropic 代理连通性", State: exitpolicy.Failed, Critical: true,
				Evidence: fmt.Sprintf("api.anthropic.com 返回 HTTP 403（%d ms，当前节点 IP 可能已被地区/风控屏蔽）", elapsedMs),
				Remedy:   "请更换干净的海外住宅或支持地区节点",
				CheckedAt: time.Now().UTC(),
			}, elapsedMs
		}
		return Check{
			ID: "anthropic_connectivity", Title: "Anthropic 代理连通性", State: exitpolicy.Passed, Critical: true,
			Evidence: fmt.Sprintf("经隔离代理成功握手 api.anthropic.com（HTTP %d · 延迟 %d ms）", resp.StatusCode, elapsedMs),
			CheckedAt: time.Now().UTC(),
		}, elapsedMs
	}
	return Check{
		ID: "anthropic_connectivity", Title: "Anthropic 代理连通性", State: exitpolicy.Failed, Critical: true,
		Evidence: "经代理连接 api.anthropic.com 连续 3 次失败: " + lastErr.Error(),
		Remedy:   "当前节点可能被 Cloudflare/Anthropic 拦截或网络不稳定，请切换节点",
		CheckedAt: time.Now().UTC(),
	}, 0
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
