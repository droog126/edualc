package accounts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type QuotaWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    string  `json:"resets_at,omitempty"`
}

type ModelQuota struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    string  `json:"resets_at,omitempty"`
}

type AccountUsage struct {
	Email            string        `json:"email,omitempty"`
	SubscriptionType string        `json:"subscription_type,omitempty"`
	PlanTier         string        `json:"plan_tier,omitempty"`
	FiveHour         *QuotaWindow  `json:"five_hour,omitempty"`
	SevenDay         *QuotaWindow  `json:"seven_day,omitempty"`
	ModelScoped      []ModelQuota  `json:"model_scoped,omitempty"`
	CheckedAt        time.Time     `json:"checked_at,omitempty"`
	Error            string        `json:"error,omitempty"`
}

const usageCacheFilename = ".edualc_usage.json"

func loadCachedUsage(dir string) *AccountUsage {
	b, err := os.ReadFile(filepath.Join(dir, usageCacheFilename))
	if err != nil {
		return nil
	}
	var u AccountUsage
	if json.Unmarshal(b, &u) != nil {
		return nil
	}
	return &u
}

func saveCachedUsage(dir string, u *AccountUsage) {
	if u == nil {
		return
	}
	if b, err := json.MarshalIndent(u, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, usageCacheFilename), b, 0o600)
	}
}

func usageEnv(accountDir, proxyURL, tz, locale string) []string {
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	if tz == "" {
		tz = "UTC"
	}
	if locale == "" {
		locale = "en_US.UTF-8"
	}
	env := []string{
		"PATH=" + pathVal,
		"HOME=" + accountDir,
		"CLAUDE_CONFIG_DIR=" + accountDir,
		"TZ=" + tz,
		"LANG=" + locale,
		"LC_ALL=" + locale,
		"NO_COLOR=1",
	}
	if proxyURL != "" {
		env = append(env,
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL,
			"https_proxy="+proxyURL,
			"NO_PROXY=localhost,127.0.0.1",
			"no_proxy=localhost,127.0.0.1",
		)
	}
	return env
}

// QueryUsage fetches account identity and zero-token 5h/7d quota windows via the official claude CLI.
func QueryUsage(ctx context.Context, claudeBin, accountDir, proxyURL, tz, locale string) (*AccountUsage, error) {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	env := usageEnv(accountDir, proxyURL, tz, locale)
	out := &AccountUsage{CheckedAt: time.Now().UTC()}

	// 1. Identity & plan tier via `claude auth status --json`
	idCtx, idCancel := context.WithTimeout(ctx, 10*time.Second)
	idCmd := exec.CommandContext(idCtx, claudeBin, "auth", "status", "--json")
	idCmd.Env = env
	idBytes, idErr := idCmd.Output()
	idCancel()
	if idErr == nil {
		var st struct {
			LoggedIn         bool   `json:"loggedIn"`
			Email            string `json:"email"`
			SubscriptionType string `json:"subscriptionType"`
			PlanTier         string `json:"planTier"`
		}
		if json.Unmarshal(idBytes, &st) == nil {
			out.Email = strings.TrimSpace(st.Email)
			out.SubscriptionType = strings.TrimSpace(st.SubscriptionType)
			out.PlanTier = strings.TrimSpace(st.PlanTier)
			if !st.LoggedIn {
				out.Error = "账号未登录"
				saveCachedUsage(accountDir, out)
				return out, errors.New("账号未登录")
			}
		}
	}

	// 2. Zero-token structured usage via SDKControlGetUsageResponse
	qCtx, qCancel := context.WithTimeout(ctx, 25*time.Second)
	defer qCancel()
	cmd := exec.CommandContext(qCtx, claudeBin,
		"--setting-sources", "user",
		"--settings", `{"disableAllHooks":true}`,
		"-p",
		"--input-format", "stream-json",
		"--verbose",
		"--output-format", "stream-json",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--tools", "",
	)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"type":"control_request","request_id":"quota","request":{"subtype":"get_usage","skip_behaviors":true}}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		out.Error = "查询用量失败: " + msg
		saveCachedUsage(accountDir, out)
		return out, fmt.Errorf("%s", out.Error)
	}

	if err := parseControlUsageOutput(stdout.Bytes(), out); err != nil {
		out.Error = err.Error()
		saveCachedUsage(accountDir, out)
		return out, err
	}
	saveCachedUsage(accountDir, out)
	return out, nil
}

func parseControlUsageOutput(raw []byte, out *AccountUsage) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 32*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var env struct {
			Type     string `json:"type"`
			Response *struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  *struct {
					SubscriptionType    string `json:"subscription_type"`
					RateLimitsAvailable bool   `json:"rate_limits_available"`
					RateLimits          *struct {
						FiveHour *struct {
							Utilization float64 `json:"utilization"`
							ResetsAt    string  `json:"resets_at"`
						} `json:"five_hour"`
						SevenDay *struct {
							Utilization float64 `json:"utilization"`
							ResetsAt    string  `json:"resets_at"`
						} `json:"seven_day"`
						ModelScoped []struct {
							DisplayName string  `json:"display_name"`
							Utilization float64 `json:"utilization"`
							ResetsAt    string  `json:"resets_at"`
						} `json:"model_scoped"`
					} `json:"rate_limits"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if env.Type != "control_response" || env.Response == nil || env.Response.RequestID != "quota" {
			continue
		}
		if env.Response.Subtype != "success" || env.Response.Response == nil {
			return errors.New("Claude CLI 未返回有效用量响应")
		}
		r := env.Response.Response
		if r.SubscriptionType != "" && out.SubscriptionType == "" {
			out.SubscriptionType = r.SubscriptionType
		}
		if !r.RateLimitsAvailable || r.RateLimits == nil {
			return errors.New("当前账号暂未返回限额窗口数据")
		}
		if r.RateLimits.FiveHour != nil {
			out.FiveHour = &QuotaWindow{
				UsedPercent: r.RateLimits.FiveHour.Utilization,
				ResetsAt:    r.RateLimits.FiveHour.ResetsAt,
			}
		}
		if r.RateLimits.SevenDay != nil {
			out.SevenDay = &QuotaWindow{
				UsedPercent: r.RateLimits.SevenDay.Utilization,
				ResetsAt:    r.RateLimits.SevenDay.ResetsAt,
			}
		}
		for _, ms := range r.RateLimits.ModelScoped {
			if ms.DisplayName != "" {
				out.ModelScoped = append(out.ModelScoped, ModelQuota{
					Name:        ms.DisplayName,
					UsedPercent: ms.Utilization,
					ResetsAt:    ms.ResetsAt,
				})
			}
		}
		return nil
	}
	return errors.New("未解析到 control_response 用量数据")
}
