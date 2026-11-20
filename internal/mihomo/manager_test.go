package mihomo

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExtractAndSanitizeStripsAllDirectRules(t *testing.T) {
	dirtySub := `
proxies:
  - name: "US-LA-01"
    type: vmess
    server: us1.example.com
    port: 443
    uuid: 11111111-1111-1111-1111-111111111111
    udp: true
  - name: "DIRECT"
    type: direct
  - name: "JP-TYO-01"
    type: trojan
    server: jp1.example.com
    port: 443
    password: secret
    udp: true
proxy-groups:
  - name: Auto
    type: select
    proxies: [DIRECT, US-LA-01]
rules:
  - DOMAIN-SUFFIX,anthropic.com,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,DIRECT
`
	proxies, err := ExtractProxiesFromClashYAML([]byte(dirtySub))
	if err != nil {
		t.Fatalf("ExtractProxiesFromClashYAML failed: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expected 2 clean proxies, got %d", len(proxies))
	}

	out, names, chain, err := BuildSanitizedConfig(proxies, "", 7890, "127.0.0.1:9090")
	if err != nil {
		t.Fatalf("BuildSanitizedConfig failed: %v", err)
	}
	if chain {
		t.Fatalf("expected chain=false")
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 node names, got %v", names)
	}
	if strings.Contains(strings.ToUpper(string(out)), "DIRECT") {
		t.Fatalf("sanitized mihomo config must NEVER contain DIRECT:\n%s", string(out))
	}

	var parsed struct {
		IPv6  bool     `yaml:"ipv6"`
		Rules []string `yaml:"rules"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("yaml unmarshal failed: %v", err)
	}
	if parsed.IPv6 {
		t.Fatalf("expected ipv6=false")
	}
	if len(parsed.Rules) != 1 || parsed.Rules[0] != "MATCH,EXIT" {
		t.Fatalf("expected single MATCH,EXIT rule, got %v", parsed.Rules)
	}
}

func TestBuildSanitizedConfigWithLandingChain(t *testing.T) {
	front, err := ParseProxyURL("FrontNode", "socks5://1.2.3.4:1080")
	if err != nil {
		t.Fatalf("ParseProxyURL failed: %v", err)
	}
	out, _, chain, err := BuildSanitizedConfig([]map[string]any{front}, "socks5://u:p@5.6.7.8:2080", 7890, "127.0.0.1:9090")
	if err != nil {
		t.Fatalf("BuildSanitizedConfig chain failed: %v", err)
	}
	if !chain {
		t.Fatalf("expected chain=true")
	}
	text := string(out)
	if !strings.Contains(text, "dialer-proxy: FRONT") {
		t.Fatalf("expected dialer-proxy: FRONT in config:\n%s", text)
	}
	if strings.Contains(strings.ToUpper(text), "DIRECT") {
		t.Fatalf("chain config must never contain DIRECT:\n%s", text)
	}
}
