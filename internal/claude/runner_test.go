package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerStreamingAndEnvIsolation(t *testing.T) {
	dir := t.TempDir()
	mockBin := filepath.Join(dir, "mock-claude.sh")
	script := `#!/bin/sh
# Verify isolated env
if [ "$TZ" != "America/Los_Angeles" ]; then
  echo "bad TZ=$TZ" >&2
  exit 1
fi
if [ -n "$RELAY_API_KEY" ]; then
  echo "leaked RELAY_API_KEY" >&2
  exit 1
fi
if [ "$HTTP_PROXY" != "http://gateway:7890" ]; then
  echo "bad HTTP_PROXY=$HTTP_PROXY" >&2
  exit 1
fi
cat >/dev/null
printf '%s\n' '{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-sonnet-4-5"}}}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello "}}}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world!"}}}'
printf '%s\n' '{"type":"result","subtype":"success","result":"Hello world!","usage":{"input_tokens":12,"output_tokens":4}}'
`
	if err := os.WriteFile(mockBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock bin: %v", err)
	}
	t.Setenv("RELAY_API_KEY", "super-secret-should-not-leak")

	runner := NewRunner(mockBin, "sonnet", nil)
	var chunks []string
	res, err := runner.Run(context.Background(), RunRequest{
		Model:        "sonnet",
		Messages:     []Message{{Role: "user", Content: "hi"}},
		AccountDir:   dir,
		ProxyURL:     "http://gateway:7890",
		SyncedTZ:     "America/Los_Angeles",
		SyncedLocale: "en_US.UTF-8",
		OnTextDelta: func(delta string) error {
			chunks = append(chunks, delta)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Runner.Run failed: %v", err)
	}
	if res.Text != "Hello world!" {
		t.Fatalf("expected 'Hello world!', got %q", res.Text)
	}
	if strings.Join(chunks, "|") != "Hello |world!" {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected usage: %+v", res.Usage)
	}
}

func TestRunnerDetectsRateLimit(t *testing.T) {
	dir := t.TempDir()
	mockBin := filepath.Join(dir, "mock-claude-rl.sh")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"result","is_error":true,"result":"You have hit your 5-hour usage limit. Rate limit reached."}'
exit 1
`
	_ = os.WriteFile(mockBin, []byte(script), 0o755)
	runner := NewRunner(mockBin, "sonnet", nil)
	_, err := runner.Run(context.Background(), RunRequest{
		Messages:   []Message{{Role: "user", Content: "hi"}},
		AccountDir: dir,
	})
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected RateLimitError, got %T: %v", err, err)
	}
}
