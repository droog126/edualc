package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type RunRequest struct {
	Model          string
	SystemPrompt   string
	Messages       []Message
	AccountDir     string
	ProxyURL       string
	SyncedTZ       string
	SyncedLocale   string
	OnTextDelta    func(delta string) error
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type RunResult struct {
	Text  string
	Usage Usage
	Model string
}

type RateLimitError struct {
	Message string
}

func (e *RateLimitError) Error() string {
	return e.Message
}

type Runner struct {
	BinaryPath   string
	DefaultModel string
	ExtraArgs    []string
	Timeout      time.Duration
}

func NewRunner(binaryPath, defaultModel string, extraArgs []string) *Runner {
	if binaryPath == "" {
		binaryPath = "claude"
	}
	if defaultModel == "" {
		defaultModel = "sonnet"
	}
	return &Runner{
		BinaryPath:   binaryPath,
		DefaultModel: defaultModel,
		ExtraArgs:    extraArgs,
		Timeout:      10 * time.Minute,
	}
}

// NormalizeModel maps common OpenAI/Anthropic alias strings to Claude Code CLI --model values.
func NormalizeModel(requested, fallback string) string {
	m := strings.TrimSpace(requested)
	if m == "" {
		return fallback
	}
	lower := strings.ToLower(m)
	// Strip optional provider prefix like "anthropic/claude-sonnet-4-5"
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		m = m[idx+1:]
		lower = strings.ToLower(m)
	}
	switch lower {
	case "claude-sonnet", "sonnet", "gpt-4o", "gpt-4", "default":
		return "sonnet"
	case "claude-opus", "opus":
		return "opus"
	case "claude-haiku", "haiku", "gpt-4o-mini":
		return "haiku"
	default:
		return m
	}
}

// BuildPrompt renders conversation history into a clean single prompt piped over stdin.
func BuildPrompt(messages []Message) (system string, prompt string) {
	var sysParts []string
	var convo []Message
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		text := msg.Content
		if role == "system" || role == "developer" {
			if strings.TrimSpace(text) != "" {
				sysParts = append(sysParts, text)
			}
			continue
		}
		convo = append(convo, Message{Role: role, Content: text})
	}
	system = strings.Join(sysParts, "\n\n")
	if len(convo) == 0 {
		return system, ""
	}
	if len(convo) == 1 && convo[0].Role == "user" {
		return system, convo[0].Content
	}
	var b strings.Builder
	b.WriteString("Below is the conversation history. Continue naturally as the assistant and answer the latest user message.\n\n")
	for _, m := range convo {
		label := "User"
		if m.Role == "assistant" {
			label = "Assistant"
		}
		b.WriteString(label)
		b.WriteString(":\n")
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	b.WriteString("Assistant:")
	return system, b.String()
}

// Run executes the real official `claude` CLI inside the isolated container and streams its output.
func (r *Runner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	model := NormalizeModel(req.Model, r.DefaultModel)
	sysFromMsgs, prompt := BuildPrompt(req.Messages)
	systemPrompt := strings.TrimSpace(req.SystemPrompt)
	if sysFromMsgs != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n" + sysFromMsgs
		} else {
			systemPrompt = sysFromMsgs
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return RunResult{}, errors.New("prompt 内容不能为空")
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--model", model,
		"--tools", "",
		"--no-session-persistence",
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	if len(r.ExtraArgs) > 0 {
		args = append(args, r.ExtraArgs...)
	}

	cmd := exec.CommandContext(runCtx, r.BinaryPath, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = buildIsolatedEnv(req)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("启动官方 claude CLI 失败 (%s): %w", r.BinaryPath, err)
	}

	res, streamErr := parseStreamJSON(stdout, req.OnTextDelta)
	waitErr := cmd.Wait()
	stderrText := strings.TrimSpace(stderrBuf.String())

	if streamErr != nil {
		if isRateLimitText(streamErr.Error()) || isRateLimitText(stderrText) {
			return res, &RateLimitError{Message: pickErrorMsg(streamErr.Error(), stderrText)}
		}
		return res, streamErr
	}
	if waitErr != nil {
		combined := pickErrorMsg(stderrText, waitErr.Error())
		if isRateLimitText(combined) || isRateLimitText(res.Text) {
			return res, &RateLimitError{Message: combined}
		}
		return res, fmt.Errorf("claude CLI 执行失败: %s", combined)
	}
	if res.Model == "" {
		res.Model = model
	}
	return res, nil
}

func buildIsolatedEnv(req RunRequest) []string {
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	tz := req.SyncedTZ
	if tz == "" {
		tz = "UTC"
	}
	lang := req.SyncedLocale
	if lang == "" {
		lang = "en_US.UTF-8"
	}
	languageEnv := strings.Split(lang, ".")[0]
	// Construct a minimal, sanitized environment: never leak host or relay API keys.
	env := []string{
		"PATH=" + pathVal,
		"HOME=" + req.AccountDir,
		"CLAUDE_CONFIG_DIR=" + req.AccountDir,
		"TZ=" + tz,
		"LANG=" + lang,
		"LC_ALL=" + lang,
		"LANGUAGE=" + languageEnv,
		"NO_COLOR=1",
		"TERM=dumb",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
	}
	if req.ProxyURL != "" {
		env = append(env,
			"HTTP_PROXY="+req.ProxyURL,
			"HTTPS_PROXY="+req.ProxyURL,
			"http_proxy="+req.ProxyURL,
			"https_proxy="+req.ProxyURL,
			"NO_PROXY=localhost,127.0.0.1",
			"no_proxy=localhost,127.0.0.1",
		)
	}
	return env
}

func parseStreamJSON(r io.Reader, onDelta func(string) error) (RunResult, error) {
	scanner := bufio.NewScanner(r)
	// Allow very large JSON lines (up to 4 MB).
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var fullText strings.Builder
	var sawPartial bool
	var res RunResult

	emit := func(chunk string) error {
		if chunk == "" {
			return nil
		}
		fullText.WriteString(chunk)
		if onDelta != nil {
			return onDelta(chunk)
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var evt struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Model   string `json:"model"`
			Usage   *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Event *struct {
				Type  string `json:"type"`
				Delta *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Message *struct {
					Model string `json:"model"`
					Usage *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
			} `json:"event"`
			Message *struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		switch evt.Type {
		case "stream_event":
			if evt.Event != nil {
				if evt.Event.Message != nil && evt.Event.Message.Model != "" {
					res.Model = evt.Event.Message.Model
				}
				if evt.Event.Type == "content_block_delta" && evt.Event.Delta != nil && evt.Event.Delta.Type == "text_delta" {
					sawPartial = true
					if err := emit(evt.Event.Delta.Text); err != nil {
						return res, err
					}
				}
			}
		case "assistant":
			if evt.Message != nil {
				if evt.Message.Model != "" {
					res.Model = evt.Message.Model
				}
				if evt.Message.Usage != nil {
					res.Usage.InputTokens = evt.Message.Usage.InputTokens
					res.Usage.OutputTokens = evt.Message.Usage.OutputTokens
				}
				if !sawPartial {
					for _, blk := range evt.Message.Content {
						if blk.Type == "text" && blk.Text != "" {
							if err := emit(blk.Text); err != nil {
								return res, err
							}
						}
					}
				}
			}
		case "result":
			if evt.Usage != nil {
				res.Usage.InputTokens = evt.Usage.InputTokens
				res.Usage.OutputTokens = evt.Usage.OutputTokens
			}
			if evt.IsError {
				msg := strings.TrimSpace(evt.Result)
				if msg == "" {
					msg = "claude 返回错误结果"
				}
				return res, errors.New(msg)
			}
			if fullText.Len() == 0 && evt.Result != "" {
				if err := emit(evt.Result); err != nil {
					return res, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}
	res.Text = fullText.String()
	return res, nil
}

func isRateLimitText(s string) bool {
	lower := strings.ToLower(s)
	keywords := []string{
		"rate limit",
		"rate_limit",
		"429",
		"usage limit",
		"limit reached",
		"hit your limit",
		"overloaded",
		"too many requests",
		"quota exceeded",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func pickErrorMsg(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}
