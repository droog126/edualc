package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

type execReq struct {
	Cmd      string            `json:"cmd"`
	Args     []string          `json:"args"`
	Cwd      string            `json:"cwd"`
	Env      map[string]string `json:"env,omitempty"`
	StdinB64 string            `json:"stdin_b64,omitempty"`
}

type streamEvent struct {
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

func collectForwardedEnv() map[string]string {
	prefixes := []string{"CARGO_", "RUST", "LK2_", "BEVY_", "NEXTEST_", "WGPU_", "RAYON_"}
	exact := map[string]bool{"NO_COLOR": true, "TERM": true, "CI": true, "COLORTERM": true, "TMPDIR": true}
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if exact[k] {
			out[k] = v
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
				break
			}
		}
	}
	return out
}

func readPipedStdin() string {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return ""
	}
	// Only read stdin when piped or redirected from a file (not an interactive TTY).
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil || len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

func main() {
	cmdName := filepath.Base(os.Args[0])
	args := os.Args[1:]
	if cmdName == "hostshim" {
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: hostshim <cmd> [args...]")
			os.Exit(2)
		}
		cmdName = args[0]
		args = args[1:]
	}

	cwd, _ := os.Getwd()
	endpoint := strings.TrimSpace(os.Getenv("HOSTEXEC_URL"))
	if endpoint == "" {
		endpoint = "http://gateway:18082/exec"
	}
	token := strings.TrimSpace(os.Getenv("HOSTEXEC_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("INTERNAL_TOKEN"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	body, _ := json.Marshal(execReq{
		Cmd:      cmdName,
		Args:     args,
		Cwd:      cwd,
		Env:      collectForwardedEnv(),
		StdinB64: readPipedStdin(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hostshim] request error: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-HostExec-Token", token)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "[hostshim] 无法连接宿主机命令守护进程 (%s): %v\n请确认宿主机已启动 scripts/hostexec.sh (或 start.sh)\n", endpoint, err)
		os.Exit(127)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(os.Stderr, "[hostshim] 宿主机拒绝执行 (HTTP %d): %s\n", resp.StatusCode, strings.TrimSpace(string(msg)))
		os.Exit(1)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	exitCode := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev streamEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Stream {
		case "stdout":
			_, _ = io.WriteString(os.Stdout, ev.Data)
		case "stderr":
			_, _ = io.WriteString(os.Stderr, ev.Data)
		}
		if ev.Error != "" {
			fmt.Fprintf(os.Stderr, "[hostshim] %s\n", ev.Error)
		}
		if ev.ExitCode != nil {
			exitCode = *ev.ExitCode
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "[hostshim] stream error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(exitCode)
}
