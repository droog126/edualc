package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type resumeSchedule struct {
	Enabled     bool   `json:"enabled"`
	IntervalMin int    `json:"interval_min"` // 5 / 10 / 15 / 30 / 60
	MaxRetries  int    `json:"max_retries"`  // per stalled file-size fingerprint
	Prompt      string `json:"prompt"`
	Concurrency int    `json:"concurrency"`
}

type scannedSession struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	MtimeStr string `json:"mtime_str"`
	SizeKB   int    `json:"size_kb"`
}

type resumeManager struct {
	mu           sync.Mutex
	running      bool
	cmd          *exec.Cmd
	startedAt    time.Time
	finished     time.Time
	lastExit     int
	logs         []string
	schedule     resumeSchedule
	scheduleFile string
	nextCheckAt  time.Time
	retryCounts  map[string]int // key: "<sid>:<size_kb>" -> attempts
}

var globalResume = &resumeManager{
	scheduleFile: filepath.Join("data", "runtime", "resume_schedule.json"),
	schedule: resumeSchedule{
		Enabled:     false,
		IntervalMin: 10,
		MaxRetries:  2,
		Prompt:      "继续完成上面中断的任务，并给出简明的最终结果总结。",
		Concurrency: 1,
	},
	retryCounts: make(map[string]int),
}

func initResumeScheduler() {
	globalResume.loadSchedule()
	go globalResume.schedulerLoop()
}

func (rm *resumeManager) loadSchedule() {
	b, err := os.ReadFile(rm.scheduleFile)
	if err != nil {
		return
	}
	var sc resumeSchedule
	if json.Unmarshal(b, &sc) == nil {
		if sc.IntervalMin < 2 {
			sc.IntervalMin = 10
		}
		if sc.MaxRetries < 1 {
			sc.MaxRetries = 2
		}
		if sc.Concurrency < 1 {
			sc.Concurrency = 1
		}
		if strings.TrimSpace(sc.Prompt) == "" {
			sc.Prompt = "继续完成上面中断的任务，并给出简明的最终结果总结。"
		}
		rm.mu.Lock()
		rm.schedule = sc
		if sc.Enabled {
			rm.nextCheckAt = time.Now().UTC().Add(time.Duration(sc.IntervalMin) * time.Minute)
		}
		rm.mu.Unlock()
	}
}

func (rm *resumeManager) saveScheduleLocked() {
	_ = os.MkdirAll(filepath.Dir(rm.scheduleFile), 0o755)
	if b, err := json.MarshalIndent(rm.schedule, "", "  "); err == nil {
		_ = os.WriteFile(rm.scheduleFile, b, 0o644)
	}
}

func (rm *resumeManager) updateSchedule(sc resumeSchedule) resumeSchedule {
	if sc.IntervalMin < 2 {
		sc.IntervalMin = 2
	}
	if sc.IntervalMin > 240 {
		sc.IntervalMin = 240
	}
	if sc.MaxRetries < 1 {
		sc.MaxRetries = 1
	}
	if sc.MaxRetries > 5 {
		sc.MaxRetries = 5
	}
	if sc.Concurrency < 1 || sc.Concurrency > 2 {
		sc.Concurrency = 1
	}
	if strings.TrimSpace(sc.Prompt) == "" {
		sc.Prompt = "继续完成上面中断的任务，并给出简明的最终结果总结。"
	}
	rm.mu.Lock()
	rm.schedule = sc
	if sc.Enabled {
		rm.nextCheckAt = time.Now().UTC().Add(time.Duration(sc.IntervalMin) * time.Minute)
	} else {
		rm.nextCheckAt = time.Time{}
	}
	rm.saveScheduleLocked()
	out := rm.schedule
	rm.mu.Unlock()
	if sc.Enabled {
		rm.appendLog(fmt.Sprintf("[schedule] 已开启定时自动续跑：每 %d 分钟巡检一次（单会话上限 %d 次）", sc.IntervalMin, sc.MaxRetries))
	} else {
		rm.appendLog("[schedule] 已关闭定时自动续跑")
	}
	return out
}

func (rm *resumeManager) schedulerLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		rm.mu.Lock()
		enabled := rm.schedule.Enabled
		running := rm.running
		due := !rm.nextCheckAt.IsZero() && time.Now().UTC().After(rm.nextCheckAt)
		sc := rm.schedule
		rm.mu.Unlock()

		if !enabled || running || !due {
			continue
		}

		// Schedule next check window
		rm.mu.Lock()
		rm.nextCheckAt = time.Now().UTC().Add(time.Duration(sc.IntervalMin) * time.Minute)
		rm.mu.Unlock()

		sessions := scanSessionsViaPython()
		var eligible []string
		rm.mu.Lock()
		for _, s := range sessions {
			if s.Status != "STALLED" {
				continue
			}
			fp := fmt.Sprintf("%s:%d", s.ID, s.SizeKB)
			if rm.retryCounts[fp] >= sc.MaxRetries {
				continue
			}
			rm.retryCounts[fp]++
			eligible = append(eligible, s.ID)
		}
		rm.mu.Unlock()

		if len(eligible) == 0 {
			continue
		}
		rm.appendLog(fmt.Sprintf("[schedule] 定时巡检发现 %d 个可自动续跑的中断会话，开始执行...", len(eligible)))
		if err := rm.start(eligible, false, sc.Prompt, sc.Concurrency); err != nil {
			rm.appendLog("[schedule] 跳过本轮: " + err.Error())
		}
	}
}

func scanSessionsViaPython() []scannedSession {
	cmd := exec.Command(pythonBinary(), filepath.Join("scripts", "batch-resume.py"), "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var parsed struct {
		Sessions []scannedSession `json:"sessions"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		return nil
	}
	return parsed.Sessions
}

func pythonBinary() string {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("python"); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return "python3"
}

func (rm *resumeManager) appendLog(line string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.logs = append(rm.logs, line)
	if len(rm.logs) > 200 {
		rm.logs = rm.logs[len(rm.logs)-200:]
	}
}

func (rm *resumeManager) snapshot() map[string]any {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	logText := strings.Join(rm.logs, "\n")
	out := map[string]any{
		"running":   rm.running,
		"last_exit": rm.lastExit,
		"log":       logText,
		"schedule":  rm.schedule,
	}
	if !rm.startedAt.IsZero() {
		out["started_at"] = rm.startedAt.UTC().Format(time.RFC3339)
	}
	if !rm.finished.IsZero() {
		out["finished_at"] = rm.finished.UTC().Format(time.RFC3339)
	}
	if !rm.nextCheckAt.IsZero() && rm.schedule.Enabled {
		out["next_check_at"] = rm.nextCheckAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (rm *resumeManager) start(ids []string, runAll bool, prompt string, concurrency int) error {
	rm.mu.Lock()
	if rm.running {
		rm.mu.Unlock()
		return errors.New("已有批量续跑任务正在执行，请稍候或先点击停止")
	}
	scriptPath := filepath.Join("scripts", "batch-resume.py")
	args := []string{"-u", scriptPath}
	if concurrency > 1 {
		args = append(args, "--concurrency", strconv.Itoa(concurrency))
	}
	if strings.TrimSpace(prompt) != "" {
		args = append(args, "--prompt", strings.TrimSpace(prompt))
	}
	if runAll {
		args = append(args, "--all")
	} else if len(ids) > 0 {
		args = append(args, ids...)
	} else {
		rm.mu.Unlock()
		return errors.New("请指定要续跑的会话或选择续跑全部")
	}

	cmd := exec.Command(pythonBinary(), args...)
	cmd.Env = os.Environ()
	configureCmdGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		rm.mu.Unlock()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		rm.mu.Unlock()
		return err
	}

	rm.running = true
	rm.cmd = cmd
	rm.startedAt = time.Now().UTC()
	rm.finished = time.Time{}
	rm.lastExit = 0
	rm.logs = append(rm.logs, "[ui] 启动批量续跑任务...")
	if len(rm.logs) > 200 {
		rm.logs = rm.logs[len(rm.logs)-200:]
	}
	rm.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 16*1024), 512*1024)
		for sc.Scan() {
			rm.appendLog(sc.Text())
		}
		code := 0
		if wErr := cmd.Wait(); wErr != nil {
			var ee *exec.ExitError
			if errors.As(wErr, &ee) {
				code = ee.ExitCode()
			} else {
				code = 1
			}
		}
		rm.mu.Lock()
		rm.running = false
		rm.cmd = nil
		rm.lastExit = code
		rm.finished = time.Now().UTC()
		rm.mu.Unlock()
		if code == 0 {
			rm.appendLog("[ui] 批量续跑任务已全部完成。")
		} else {
			rm.appendLog("[ui] 任务结束（exit=" + strconv.Itoa(code) + "）。")
		}
	}()
	return nil
}

func (rm *resumeManager) stop() bool {
	rm.mu.Lock()
	cmd := rm.cmd
	wasRunning := rm.running
	rm.mu.Unlock()
	if wasRunning && cmd != nil {
		terminateCmdGroup(cmd)
		rm.appendLog("[ui] 已发送停止信号...")
		return true
	}
	return false
}

func handleResumeStatus(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	res := globalResume.snapshot()
	cmd := exec.Command(pythonBinary(), filepath.Join("scripts", "batch-resume.py"), "--json")
	if out, err := cmd.Output(); err == nil {
		var parsed struct {
			Sessions     []any `json:"sessions"`
			StalledCount int   `json:"stalled_count"`
		}
		if json.Unmarshal(out, &parsed) == nil {
			res["sessions"] = parsed.Sessions
			res["stalled_count"] = parsed.StalledCount
		}
	}
	if _, ok := res["sessions"]; !ok {
		res["sessions"] = []any{}
		res["stalled_count"] = 0
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(res)
}

func handleResumeRun(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		IDs         []string `json:"ids"`
		All         bool     `json:"all"`
		Prompt      string   `json:"prompt"`
		Concurrency int      `json:"concurrency"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := globalResume.start(req.IDs, req.All, req.Prompt, req.Concurrency); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	handleResumeStatus(w, r, cfg)
}

func handleResumeStop(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	globalResume.stop()
	handleResumeStatus(w, r, cfg)
}

func handleResumeSchedule(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var sc resumeSchedule
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&sc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	globalResume.updateSchedule(sc)
	handleResumeStatus(w, r, cfg)
}
