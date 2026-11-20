package exitpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
	_ "time/tzdata"
)

// QueryExitIP fetches the public IPv4 observed through a proxy-only HTTP client.
func QueryExitIP(ctx context.Context, client *http.Client) (string, error) {
	endpoints := []struct {
		name string
		url  string
	}{
		{"icanhazip", "https://ipv4.icanhazip.com/"},
		{"ipify", "https://api.ipify.org/"},
		{"ifconfig.me", "https://ifconfig.me/ip"},
	}
	var errs []string
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		ip, err := fetchPlainIP(ctx, client, ep.url)
		if err == nil {
			return ip, nil
		}
		errs = append(errs, ep.name+": "+err.Error())
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", fmt.Errorf("通过代理查询公网 IPv4 失败（%s）", strings.Join(errs, "；"))
}

func fetchPlainIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8.7.1")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return "", errors.New("返回内容不是有效公网 IPv4")
	}
	return addr.String(), nil
}

// Lookup queries geolocation for a measured exit IPv4 via the proxy-only HTTP client.
func Lookup(ctx context.Context, client *http.Client, ipStr string) (Location, error) {
	addr := net.ParseIP(strings.TrimSpace(ipStr))
	if addr == nil || addr.To4() == nil {
		return Location{}, errors.New("无效的 IPv4 地址")
	}
	providers := []struct {
		name           string
		url            string
		requireSuccess bool
	}{
		{"ipwho.is", "https://ipwho.is/" + addr.String() + "?fields=success,ip,country,country_code,region,region_code,city,timezone.id,connection.isp,connection.asn", true},
		{"IP.SB", "https://api.ip.sb/geoip/" + addr.String(), false},
		{"ipwhois.app", "https://ipwhois.app/json/" + addr.String(), true},
		{"GeoJS", "https://get.geojs.io/v1/ip/geo/" + addr.String() + ".json", false},
		{"ipapi.co", "https://ipapi.co/" + addr.String() + "/json/", false},
	}
	var errs []string
	for _, p := range providers {
		if ctx.Err() != nil {
			break
		}
		loc, err := queryGeoProvider(ctx, client, p.url, p.requireSuccess, addr)
		if err == nil {
			return loc, nil
		}
		errs = append(errs, p.name+": "+err.Error())
	}
	if ctx.Err() != nil {
		return Location{}, ctx.Err()
	}
	return Location{}, fmt.Errorf("出口地理定位失败（%s）", strings.Join(errs, "；"))
}

func queryGeoProvider(ctx context.Context, client *http.Client, endpoint string, requireSuccess bool, expected net.IP) (Location, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Location{}, err
	}
	req.Header.Set("User-Agent", "curl/8.7.1")
	resp, err := client.Do(req)
	if err != nil {
		return Location{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Location{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var raw struct {
		Success     *bool           `json:"success"`
		Error       *bool           `json:"error"`
		IP          string          `json:"ip"`
		Country     string          `json:"country"`
		CountryName string          `json:"country_name"`
		CountryCode string          `json:"country_code"`
		Region      string          `json:"region"`
		RegionCode  string          `json:"region_code"`
		City        string          `json:"city"`
		Timezone    json.RawMessage `json:"timezone"`
		ISP         string          `json:"isp"`
		Org         string          `json:"organization"`
		ASN         json.RawMessage `json:"asn"`
		Connection  *struct {
			ASN int    `json:"asn"`
			ISP string `json:"isp"`
		} `json:"connection"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&raw); err != nil {
		return Location{}, errors.New("响应 JSON 解析失败")
	}
	if (raw.Error != nil && *raw.Error) || (requireSuccess && (raw.Success == nil || !*raw.Success)) {
		return Location{}, errors.New("服务返回失败状态")
	}
	gotIP := net.ParseIP(strings.TrimSpace(raw.IP))
	if gotIP == nil || !gotIP.Equal(expected) {
		return Location{}, errors.New("返回 IP 与出口不匹配")
	}
	cc := strings.ToUpper(strings.TrimSpace(raw.CountryCode))
	if !countryCodeRe.MatchString(cc) {
		return Location{}, errors.New("缺少有效国家代码")
	}
	var tz string
	if err := json.Unmarshal(raw.Timezone, &tz); err != nil {
		var tzObj struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw.Timezone, &tzObj) == nil {
			tz = tzObj.ID
		}
	}
	tz = strings.TrimSpace(tz)
	if tz == "" || tz == "Local" {
		return Location{}, errors.New("缺少有效时区")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return Location{}, fmt.Errorf("无效 IANA 时区 %q", tz)
	}
	countryName := raw.CountryName
	if countryName == "" {
		countryName = raw.Country
	}
	isp := raw.ISP
	if isp == "" {
		isp = raw.Org
	}
	var asnStr string
	if raw.Connection != nil {
		if isp == "" {
			isp = raw.Connection.ISP
		}
		if raw.Connection.ASN > 0 {
			asnStr = fmt.Sprintf("AS%d", raw.Connection.ASN)
		}
	}
	if asnStr == "" && len(raw.ASN) > 0 {
		var s string
		if json.Unmarshal(raw.ASN, &s) == nil && s != "" {
			asnStr = s
		} else {
			var n int
			if json.Unmarshal(raw.ASN, &n) == nil && n > 0 {
				asnStr = fmt.Sprintf("AS%d", n)
			}
		}
	}
	return Location{
		IP:          expected.String(),
		Country:     countryName,
		CountryCode: cc,
		Region:      strings.TrimSpace(raw.Region),
		RegionCode:  strings.TrimSpace(raw.RegionCode),
		City:        strings.TrimSpace(raw.City),
		Timezone:    tz,
		ASN:         asnStr,
		ISP:         strings.TrimSpace(isp),
		CheckedAt:   time.Now().UTC(),
	}, nil
}
