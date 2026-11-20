package exitpolicy

import (
	_ "embed"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

//go:embed data/mainland-ipv4.txt
var mainlandIPv4 string

var mainlandPrefixes, mainlandListErr = parseMainlandPrefixes(mainlandIPv4)

// BlockedCountries contains regions unsupported or high-risk for Anthropic Claude.
var BlockedCountries = map[string]string{
	"CN": "中国大陆",
	"HK": "中国香港",
	"MO": "中国澳门",
	"RU": "俄罗斯",
	"BY": "白俄罗斯",
	"KP": "朝鲜",
	"IR": "伊朗",
	"SY": "叙利亚",
	"CU": "古巴",
}

type State string

const (
	Passed  State = "passed"
	Failed  State = "failed"
	Unknown State = "unknown"
)

type Policy struct {
	Mode        string `json:"mode"`                   // "dynamic" | "static"
	Scope       string `json:"scope"`                  // "any" | "country" | "region" | "city"
	IP          string `json:"ip,omitempty"`           // bound public IPv4 when Mode == "static"
	CountryCode string `json:"country_code,omitempty"` // ISO 3166-1 alpha-2, e.g. US, JP, SG
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
}

type Location struct {
	IP          string    `json:"ip"`
	Country     string    `json:"country"`
	CountryCode string    `json:"country_code"`
	Region      string    `json:"region"`
	RegionCode  string    `json:"region_code,omitempty"`
	City        string    `json:"city"`
	Timezone    string    `json:"timezone"`
	ASN         string    `json:"asn,omitempty"`
	ISP         string    `json:"isp,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

func parseMainlandPrefixes(data string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, line := range strings.Fields(data) {
		prefix, err := netip.ParsePrefix(line)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 8 || prefix != prefix.Masked() {
			return nil, fmt.Errorf("invalid bundled mainland prefix %q", line)
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil, errors.New("bundled mainland prefix list is empty")
	}
	return prefixes, nil
}

func DefaultPolicy() Policy {
	return Policy{
		Mode:  "dynamic",
		Scope: "any",
	}
}

func (p Policy) Normalized() Policy {
	if p.Mode == "" {
		p.Mode = "dynamic"
	}
	if p.Scope == "" {
		p.Scope = "country"
	}
	p.IP = strings.TrimSpace(p.IP)
	p.CountryCode = strings.ToUpper(strings.TrimSpace(p.CountryCode))
	p.Region = strings.Join(strings.Fields(p.Region), " ")
	p.City = strings.Join(strings.Fields(p.City), " ")
	return p
}

var countryCodeRe = regexp.MustCompile(`^[A-Z]{2}$`)

func (p Policy) Validate() error {
	p = p.Normalized()
	if p.Mode != "static" && p.Mode != "dynamic" {
		return errors.New("出口模式必须为 static 或 dynamic")
	}
	if p.Scope != "any" && p.Scope != "country" && p.Scope != "region" && p.Scope != "city" {
		return errors.New("动态出口范围必须为 any / country / region / city")
	}
	if p.IP != "" {
		addr, err := netip.ParseAddr(p.IP)
		if err != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return errors.New("静态出口必须是有效的公网 IPv4 地址")
		}
	}
	if p.CountryCode != "" && !countryCodeRe.MatchString(p.CountryCode) {
		return errors.New("国家代码应为两位大写字母，例如 US / JP / SG")
	}
	return nil
}

func (p Policy) NeedsBinding() bool {
	p = p.Normalized()
	if p.Mode == "static" {
		return p.IP == ""
	}
	if p.Scope == "any" {
		return false
	}
	if p.CountryCode == "" {
		return true
	}
	if (p.Scope == "region" || p.Scope == "city") && p.Region == "" {
		return true
	}
	return p.Scope == "city" && p.City == ""
}

func (p Policy) BindFrom(loc Location) Policy {
	p = p.Normalized()
	if state, _ := CheckExitCountry(loc.IP, &loc); state != Passed {
		return p
	}
	if p.Mode == "static" {
		p.IP = loc.IP
	}
	p.CountryCode = loc.CountryCode
	p.Region = loc.Region
	p.City = loc.City
	return p.Normalized()
}

// CheckExitCountry enforces mandatory non-mainland and Anthropic-supported region rules.
func CheckExitCountry(ip string, loc *Location) (State, string) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return Unknown, "尚未取得有效公网 IPv4 出口地址"
	}
	if mainlandListErr != nil {
		return Unknown, "大陆 IPv4 网段库加载失败，保持熔断"
	}
	for _, prefix := range mainlandPrefixes {
		if prefix.Contains(addr) {
			return Failed, fmt.Sprintf("出口 IP %s 命中中国大陆网段，禁止放行", addr.String())
		}
	}
	if loc == nil || loc.IP != addr.String() {
		return Unknown, "未能确认出口 IP 归属国家与地区，保持熔断"
	}
	cc := strings.ToUpper(strings.TrimSpace(loc.CountryCode))
	if !countryCodeRe.MatchString(cc) || cc == "ZZ" || cc == "XX" {
		return Unknown, "出口归属地国家代码无效，保持熔断"
	}
	if label, blocked := BlockedCountries[cc]; blocked {
		return Failed, fmt.Sprintf("出口地区为 %s (%s)，属于 Anthropic 限制/高风险地区，已熔断禁止放行", label, cc)
	}
	return Passed, fmt.Sprintf("出口 %s 位于允许地区 %s (%s)", addr.String(), loc.Country, cc)
}

func (p Policy) Evaluate(ip string, loc *Location) (State, string) {
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return Unknown, err.Error()
	}
	if state, msg := CheckExitCountry(ip, loc); state != Passed {
		return state, msg
	}
	if p.NeedsBinding() {
		return Unknown, "出口规则尚未绑定目标 IP/地区，请在面板确认绑定当前出口或切换为 any"
	}
	if p.Mode == "static" {
		if p.IP != ip {
			return Failed, fmt.Sprintf("当前出口 IP (%s) 偏离已绑定的静态 IP (%s)", ip, p.IP)
		}
		return Passed, fmt.Sprintf("出口 IP (%s) 与绑定静态 IP 完全一致", ip)
	}
	if p.Scope == "any" {
		return Passed, fmt.Sprintf("动态出口 %s (%s / %s) 符合允许地区要求", ip, loc.CountryCode, loc.City)
	}
	if !strings.EqualFold(p.CountryCode, loc.CountryCode) {
		return Failed, fmt.Sprintf("当前出口国家 %s 超出绑定国家 %s", loc.CountryCode, p.CountryCode)
	}
	if p.Scope == "country" {
		return Passed, fmt.Sprintf("出口位于允许的国家 %s (%s)", loc.CountryCode, loc.City)
	}
	if loc.Region == "" {
		return Unknown, "地理定位缺少州/省信息，无法验证 region 策略"
	}
	if !strings.EqualFold(p.Region, strings.Join(strings.Fields(loc.Region), " ")) {
		return Failed, fmt.Sprintf("当前出口州/省 (%s) 超出绑定范围 (%s)", loc.Region, p.Region)
	}
	if p.Scope == "region" {
		return Passed, fmt.Sprintf("出口位于允许的州/省 %s, %s", loc.Region, loc.CountryCode)
	}
	if loc.City == "" {
		return Unknown, "地理定位缺少城市信息，无法验证 city 策略"
	}
	if !strings.EqualFold(p.City, strings.Join(strings.Fields(loc.City), " ")) {
		return Failed, fmt.Sprintf("当前出口城市 (%s) 超出绑定范围 (%s)", loc.City, p.City)
	}
	return Passed, fmt.Sprintf("出口位于允许的城市 %s, %s, %s", loc.City, loc.Region, loc.CountryCode)
}

// LocaleProfile holds POSIX and browser language settings matched to an exit country.
type LocaleProfile struct {
	POSIX           string `json:"posix"`            // e.g. ja_JP.UTF-8
	BCP47           string `json:"bcp47"`            // e.g. ja-JP
	AcceptLanguages string `json:"accept_languages"` // e.g. ja-JP,ja,en-US,en
	LanguageEnv     string `json:"language_env"`     // e.g. ja_JP:ja:en_US:en
}

// LocaleProfileForCountry returns realistic OS + browser language settings aligned with the exit country.
func LocaleProfileForCountry(countryCode string) LocaleProfile {
	switch strings.ToUpper(strings.TrimSpace(countryCode)) {
	case "US":
		return LocaleProfile{"en_US.UTF-8", "en-US", "en-US,en", "en_US:en"}
	case "GB":
		return LocaleProfile{"en_GB.UTF-8", "en-GB", "en-GB,en-US,en", "en_GB:en"}
	case "CA":
		return LocaleProfile{"en_CA.UTF-8", "en-CA", "en-CA,en-US,en", "en_CA:en"}
	case "AU":
		return LocaleProfile{"en_AU.UTF-8", "en-AU", "en-AU,en-GB,en", "en_AU:en"}
	case "NZ":
		return LocaleProfile{"en_NZ.UTF-8", "en-NZ", "en-NZ,en-GB,en", "en_NZ:en"}
	case "IE":
		return LocaleProfile{"en_IE.UTF-8", "en-IE", "en-IE,en-GB,en", "en_IE:en"}
	case "SG":
		return LocaleProfile{"en_SG.UTF-8", "en-SG", "en-SG,en-US,en", "en_SG:en"}
	case "IN":
		return LocaleProfile{"en_IN.UTF-8", "en-IN", "en-IN,en-GB,en", "en_IN:en"}
	case "JP":
		return LocaleProfile{"ja_JP.UTF-8", "ja-JP", "ja-JP,ja,en-US,en", "ja_JP:ja:en_US:en"}
	case "KR":
		return LocaleProfile{"ko_KR.UTF-8", "ko-KR", "ko-KR,ko,en-US,en", "ko_KR:ko:en_US:en"}
	case "TW":
		return LocaleProfile{"zh_TW.UTF-8", "zh-TW", "zh-TW,zh,en-US,en", "zh_TW:zh:en_US:en"}
	case "DE", "AT":
		return LocaleProfile{"de_DE.UTF-8", "de-DE", "de-DE,de,en-US,en", "de_DE:de:en_US:en"}
	case "CH":
		return LocaleProfile{"de_CH.UTF-8", "de-CH", "de-CH,de,en-US,en", "de_CH:de:en_US:en"}
	case "FR", "BE":
		return LocaleProfile{"fr_FR.UTF-8", "fr-FR", "fr-FR,fr,en-US,en", "fr_FR:fr:en_US:en"}
	case "NL":
		return LocaleProfile{"nl_NL.UTF-8", "nl-NL", "nl-NL,nl,en-US,en", "nl_NL:nl:en_US:en"}
	case "IT":
		return LocaleProfile{"it_IT.UTF-8", "it-IT", "it-IT,it,en-US,en", "it_IT:it:en_US:en"}
	case "ES":
		return LocaleProfile{"es_ES.UTF-8", "es-ES", "es-ES,es,en-US,en", "es_ES:es:en_US:en"}
	case "BR":
		return LocaleProfile{"pt_BR.UTF-8", "pt-BR", "pt-BR,pt,en-US,en", "pt_BR:pt:en_US:en"}
	case "SE":
		return LocaleProfile{"sv_SE.UTF-8", "sv-SE", "sv-SE,sv,en-US,en", "sv_SE:sv:en_US:en"}
	case "PL":
		return LocaleProfile{"pl_PL.UTF-8", "pl-PL", "pl-PL,pl,en-US,en", "pl_PL:pl:en_US:en"}
	default:
		return LocaleProfile{"en_US.UTF-8", "en-US", "en-US,en", "en_US:en"}
	}
}

// LocaleForCountry returns a realistic POSIX locale aligned with the exit country.
func LocaleForCountry(countryCode string) string {
	return LocaleProfileForCountry(countryCode).POSIX
}
