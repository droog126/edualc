package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	ExitGroupName   = "EXIT"
	FrontGroupName  = "FRONT"
	LandingNodeName = "LANDING_RESIDENTIAL"
	StaticNodeName  = "STATIC_EXIT"
)

var forbiddenProxyNames = map[string]bool{
	"DIRECT":      true,
	"REJECT":      true,
	"REJECT-DROP": true,
	"PASS":        true,
	"COMPATIBLE":  true,
	"GLOBAL":      true,
}

type State struct {
	Mode            string    `json:"mode"` // "unconfigured" | "static" | "subscription" | "manual_yaml"
	SubscriptionURL string    `json:"subscription_url,omitempty"`
	StaticProxyURL  string    `json:"static_proxy_url,omitempty"`
	LandingProxyURL string    `json:"landing_proxy_url,omitempty"`
	SelectedNode    string    `json:"selected_node,omitempty"`
	Nodes           []string  `json:"nodes"`
	ChainEnabled    bool      `json:"chain_enabled"`
	ZeroDirectRules bool      `json:"zero_direct_rules"`
	IPv6Disabled    bool      `json:"ipv6_disabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Status struct {
	State
	Running        bool   `json:"running"`
	MihomoPort     int    `json:"mihomo_port"`
	MihomoVersion  string `json:"mihomo_version,omitempty"`
	ControllerAddr string `json:"controller_addr"`
	ActiveGroup    string `json:"active_group"`
	LastError      string `json:"last_error,omitempty"`
}

type Manager struct {
	mu             sync.Mutex
	dataDir        string
	binaryPath     string
	mihomoVersion  string
	mixedPort      int
	controllerAddr string
	state          State
	rawProxies     []map[string]any
	cmd            *exec.Cmd
	running        bool
	lastError      string
	httpClient     *http.Client
}

func NewManager(dataDir, binaryPath string, mixedPort int, controllerAddr string) *Manager {
	if mixedPort <= 0 {
		mixedPort = 7890
	}
	if controllerAddr == "" {
		controllerAddr = "127.0.0.1:9090"
	}
	if binaryPath == "" {
		binaryPath = "mihomo"
	}
	m := &Manager{
		dataDir:        dataDir,
		binaryPath:     binaryPath,
		mixedPort:      mixedPort,
		controllerAddr: controllerAddr,
		state: State{
			Mode:            "unconfigured",
			ZeroDirectRules: true,
			IPv6Disabled:    true,
		},
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	_ = m.loadPersisted()
	if out, err := exec.Command(m.binaryPath, "-v").CombinedOutput(); err == nil {
		m.mihomoVersion = strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	return m
}

func (m *Manager) configPath() string {
	return filepath.Join(m.dataDir, "mihomo.yaml")
}

func (m *Manager) statePath() string {
	return filepath.Join(m.dataDir, "state.json")
}

func (m *Manager) proxiesPath() string {
	return filepath.Join(m.dataDir, "proxies.json")
}

func (m *Manager) loadPersisted() error {
	if b, err := os.ReadFile(m.statePath()); err == nil {
		var st State
		if json.Unmarshal(b, &st) == nil {
			m.state = st
		}
	}
	if b, err := os.ReadFile(m.proxiesPath()); err == nil {
		var proxies []map[string]any
		if json.Unmarshal(b, &proxies) == nil {
			m.rawProxies = proxies
		}
	}
	return nil
}

func (m *Manager) savePersisted() error {
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return err
	}
	sb, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.statePath(), sb, 0o600); err != nil {
		return err
	}
	pb, err := json.Marshal(m.rawProxies)
	if err != nil {
		return err
	}
	return os.WriteFile(m.proxiesPath(), pb, 0o600)
}

// ParseProxyURL converts a socks5:// or http:// URL into a Mihomo proxy map.
func ParseProxyURL(name, rawURL string) (map[string]any, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("代理地址不能为空")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("代理地址解析失败: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	var mType string
	var tlsEnabled bool
	switch scheme {
	case "socks5", "socks5h":
		mType = "socks5"
	case "http":
		mType = "http"
	case "https":
		mType = "http"
		tlsEnabled = true
	default:
		return nil, fmt.Errorf("不支持的静态代理协议 %q（仅支持 socks5 / http / https）", u.Scheme)
	}
	host := u.Hostname()
	portStr := u.Port()
	if host == "" || portStr == "" {
		return nil, errors.New("代理地址必须包含主机和端口，例如 socks5://user:pass@1.2.3.4:1080")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("无效代理端口: %q", portStr)
	}
	node := map[string]any{
		"name":   name,
		"type":   mType,
		"server": host,
		"port":   port,
		"udp":    false,
	}
	if tlsEnabled {
		node["tls"] = true
	}
	if u.User != nil {
		if user := u.User.Username(); user != "" {
			node["username"] = user
		}
		if pass, ok := u.User.Password(); ok {
			node["password"] = pass
		}
	}
	return node, nil
}

// ExtractProxiesFromClashYAML parses a Clash/Mihomo YAML document and keeps only safe proxy entries.
func ExtractProxiesFromClashYAML(rawYAML []byte) ([]map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(rawYAML, &doc); err != nil {
		return nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	rawList, ok := doc["proxies"]
	if !ok {
		return nil, errors.New("配置中未找到 proxies 列表（请确认是 Clash / Mihomo YAML 订阅）")
	}
	slice, ok := rawList.([]any)
	if !ok || len(slice) == 0 {
		return nil, errors.New("proxies 列表为空")
	}
	seen := make(map[string]bool)
	var out []map[string]any
	for _, item := range slice {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		pType, _ := m["type"].(string)
		pType = strings.ToLower(strings.TrimSpace(pType))
		if name == "" || pType == "" || forbiddenProxyNames[strings.ToUpper(name)] {
			continue
		}
		if pType == "direct" || pType == "reject" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		cloned := make(map[string]any, len(m)+1)
		for k, v := range m {
			cloned[k] = v
		}
		// Force UDP off on proxy nodes so UDP traffic cannot sneak out.
		cloned["udp"] = false
		out = append(out, cloned)
	}
	if len(out) == 0 {
		return nil, errors.New("未提取到任何有效的代理节点")
	}
	return out, nil
}

// BuildSanitizedConfig builds a strict fail-closed Mihomo config with ZERO DIRECT rules.
func BuildSanitizedConfig(proxies []map[string]any, landingProxyURL string, mixedPort int, controllerAddr string) ([]byte, []string, bool, error) {
	if len(proxies) == 0 {
		return nil, nil, false, errors.New("至少需要一个有效代理节点")
	}
	finalProxies := make([]map[string]any, 0, len(proxies)+1)
	nodeNames := make([]string, 0, len(proxies))
	for _, p := range proxies {
		cloned := make(map[string]any, len(p))
		for k, v := range p {
			cloned[k] = v
		}
		name, _ := cloned["name"].(string)
		if name == "" || forbiddenProxyNames[strings.ToUpper(name)] {
			continue
		}
		cloned["udp"] = false
		finalProxies = append(finalProxies, cloned)
		nodeNames = append(nodeNames, name)
	}
	if len(nodeNames) == 0 {
		return nil, nil, false, errors.New("清洗后无可用代理节点")
	}

	var groups []map[string]any
	chainEnabled := false
	landingProxyURL = strings.TrimSpace(landingProxyURL)
	if landingProxyURL != "" {
		landingNode, err := ParseProxyURL(LandingNodeName, landingProxyURL)
		if err != nil {
			return nil, nil, false, fmt.Errorf("落地链式代理配置无效: %w", err)
		}
		landingNode["dialer-proxy"] = FrontGroupName
		finalProxies = append(finalProxies, landingNode)
		groups = []map[string]any{
			{
				"name":    FrontGroupName,
				"type":    "select",
				"proxies": nodeNames,
			},
			{
				"name":    ExitGroupName,
				"type":    "select",
				"proxies": []string{LandingNodeName},
			},
		}
		chainEnabled = true
	} else {
		groups = []map[string]any{
			{
				"name":    ExitGroupName,
				"type":    "select",
				"proxies": nodeNames,
			},
		}
	}

	// Strictly zero DIRECT rules: every single request MUST go through EXIT.
	cfg := map[string]any{
		"mixed-port":          mixedPort,
		"allow-lan":           true,
		"bind-address":        "*",
		"mode":                "rule",
		"log-level":           "warning",
		"ipv6":                false,
		"external-controller": controllerAddr,
		"proxies":             finalProxies,
		"proxy-groups":        groups,
		"rules": []string{
			"MATCH," + ExitGroupName,
		},
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, nil, false, err
	}
	return out, nodeNames, chainEnabled, nil
}

// FetchSubscription downloads a Clash/Mihomo YAML subscription URL from gateway's public network.
func (m *Manager) FetchSubscription(ctx context.Context, subURL string) ([]byte, error) {
	subURL = strings.TrimSpace(subURL)
	if !strings.HasPrefix(subURL, "http://") && !strings.HasPrefix(subURL, "https://") {
		return nil, errors.New("订阅链接必须以 http:// 或 https:// 开头")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash.meta/1.19.0 mihomo")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (m *Manager) ApplyStaticProxy(ctx context.Context, proxyURL string) (Status, error) {
	node, err := ParseProxyURL(StaticNodeName, proxyURL)
	if err != nil {
		return m.Status(), err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(ctx, []map[string]any{node}, "static", "", proxyURL, "")
}

func (m *Manager) ApplySubscription(ctx context.Context, subURL, rawYAML, landingProxyURL string) (Status, error) {
	var data []byte
	var err error
	mode := "manual_yaml"
	if strings.TrimSpace(rawYAML) != "" {
		data = []byte(rawYAML)
	} else {
		mode = "subscription"
		data, err = m.FetchSubscription(ctx, subURL)
		if err != nil {
			return m.Status(), err
		}
	}
	proxies, err := ExtractProxiesFromClashYAML(data)
	if err != nil {
		return m.Status(), err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(ctx, proxies, mode, subURL, "", landingProxyURL)
}

func (m *Manager) applyLocked(ctx context.Context, proxies []map[string]any, mode, subURL, staticURL, landingURL string) (Status, error) {
	yamlBytes, nodeNames, chainEnabled, err := BuildSanitizedConfig(proxies, landingURL, m.mixedPort, m.controllerAddr)
	if err != nil {
		return m.statusLocked(), err
	}
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return m.statusLocked(), err
	}
	if err := os.WriteFile(m.configPath(), yamlBytes, 0o600); err != nil {
		return m.statusLocked(), err
	}
	selected := nodeNames[0]
	for _, n := range nodeNames {
		if n == m.state.SelectedNode {
			selected = n
			break
		}
	}
	m.rawProxies = proxies
	m.state = State{
		Mode:            mode,
		SubscriptionURL: subURL,
		StaticProxyURL:  staticURL,
		LandingProxyURL: landingURL,
		SelectedNode:    selected,
		Nodes:           nodeNames,
		ChainEnabled:    chainEnabled,
		ZeroDirectRules: true,
		IPv6Disabled:    true,
		UpdatedAt:       time.Now().UTC(),
	}
	if err := m.savePersisted(); err != nil {
		return m.statusLocked(), err
	}
	if err := m.restartMihomoLocked(ctx); err != nil {
		m.lastError = err.Error()
		return m.statusLocked(), err
	}
	m.lastError = ""
	// Apply previously selected node if not the first one.
	if selected != nodeNames[0] {
		_ = m.selectInControllerLocked(ctx, selected)
	}
	return m.statusLocked(), nil
}

func (m *Manager) StartIfConfigured(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Mode == "unconfigured" {
		return nil
	}
	if _, err := os.Stat(m.configPath()); err != nil {
		if len(m.rawProxies) > 0 {
			yamlBytes, _, _, bErr := BuildSanitizedConfig(m.rawProxies, m.state.LandingProxyURL, m.mixedPort, m.controllerAddr)
			if bErr != nil {
				return bErr
			}
			_ = os.WriteFile(m.configPath(), yamlBytes, 0o600)
		} else {
			return err
		}
	}
	if err := m.restartMihomoLocked(ctx); err != nil {
		m.lastError = err.Error()
		return err
	}
	if m.state.SelectedNode != "" {
		_ = m.selectInControllerLocked(ctx, m.state.SelectedNode)
	}
	return nil
}

func (m *Manager) restartMihomoLocked(ctx context.Context) error {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_, _ = m.cmd.Process.Wait()
		m.cmd = nil
		m.running = false
	}
	bin, err := exec.LookPath(m.binaryPath)
	if err != nil {
		return fmt.Errorf("未找到 mihomo 可执行文件 (%s): %w", m.binaryPath, err)
	}
	cmd := exec.Command(bin, "-d", m.dataDir, "-f", m.configPath())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 mihomo 失败: %w", err)
	}
	m.cmd = cmd
	m.running = true

	// Wait briefly for Mihomo controller API to respond.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.controllerAddr+"/version", nil)
		resp, err := m.httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("mihomo 已启动但控制接口未在 5 秒内就绪")
}

func (m *Manager) activeSelectableGroupLocked() string {
	if m.state.ChainEnabled {
		return FrontGroupName
	}
	return ExitGroupName
}

func (m *Manager) selectInControllerLocked(ctx context.Context, node string) error {
	group := m.activeSelectableGroupLocked()
	payload, _ := json.Marshal(map[string]string{"name": node})
	endpoint := fmt.Sprintf("http://%s/proxies/%s", m.controllerAddr, url.PathEscape(group))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("切换节点失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (m *Manager) SelectNode(ctx context.Context, node string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return m.statusLocked(), errors.New("mihomo 尚未运行，请先导入订阅或配置静态代理")
	}
	found := false
	for _, n := range m.state.Nodes {
		if n == node {
			found = true
			break
		}
	}
	if !found {
		return m.statusLocked(), fmt.Errorf("未找到节点 %q", node)
	}
	if err := m.selectInControllerLocked(ctx, node); err != nil {
		return m.statusLocked(), err
	}
	m.state.SelectedNode = node
	m.state.UpdatedAt = time.Now().UTC()
	_ = m.savePersisted()
	return m.statusLocked(), nil
}

// TestDelay measures proxy latency via Mihomo's controller API.
func (m *Manager) TestDelay(ctx context.Context, node string) (int, error) {
	m.mu.Lock()
	running := m.running
	ctrl := m.controllerAddr
	m.mu.Unlock()
	if !running {
		return 0, errors.New("mihomo 尚未运行")
	}
	endpoint := fmt.Sprintf("http://%s/proxies/%s/delay?timeout=5000&url=%s",
		ctrl, url.PathEscape(node), url.QueryEscape("https://www.gstatic.com/generate_204"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var res struct {
		Delay   int    `json:"delay"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&res)
	if resp.StatusCode != http.StatusOK {
		if res.Message != "" {
			return 0, errors.New(res.Message)
		}
		return 0, fmt.Errorf("测速失败 HTTP %d", resp.StatusCode)
	}
	return res.Delay, nil
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *Manager) statusLocked() Status {
	return Status{
		State:          m.state,
		Running:        m.running,
		MihomoPort:     m.mixedPort,
		MihomoVersion:  m.mihomoVersion,
		ControllerAddr: m.controllerAddr,
		ActiveGroup:    m.activeSelectableGroupLocked(),
		LastError:      m.lastError,
	}
}
