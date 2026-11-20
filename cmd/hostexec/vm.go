package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type vmInfo struct {
	Platform   string `json:"platform"`
	Engine     string `json:"engine"`
	HostCPUs   int    `json:"host_cpus"`
	HostRAMGB  int    `json:"host_ram_gb"`
	CPU        int    `json:"cpu"`
	MemoryGB   int    `json:"memory_gb"`
	DiskGB     int    `json:"disk_gb"`
	Configurable bool `json:"configurable"`
}

func detectHostRAMGB() int {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if bytesVal, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(bytesVal / (1024 * 1024 * 1024))
			}
		}
	}
	return 0
}

func readColimaYAMLInts() (cpu, mem, disk int) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, 0, 0
	}
	b, err := os.ReadFile(filepath.Join(home, ".colima", "default", "colima.yaml"))
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cpu:") {
			cpu, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "cpu:")))
		} else if strings.HasPrefix(trimmed, "memory:") {
			mem, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "memory:")))
		} else if strings.HasPrefix(trimmed, "disk:") {
			disk, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "disk:")))
		}
	}
	return cpu, mem, disk
}

func getVMInfo() vmInfo {
	info := vmInfo{
		Platform:     runtime.GOOS,
		HostCPUs:     runtime.NumCPU(),
		HostRAMGB:    detectHostRAMGB(),
		CPU:          envInt("VM_CPU", 4),
		MemoryGB:     envInt("VM_MEMORY_GB", 6),
		DiskGB:       envInt("VM_DISK_GB", 40),
		Configurable: true,
	}
	switch runtime.GOOS {
	case "darwin":
		info.Engine = "colima (macOS Virtualization.Framework)"
		if c, m, d := readColimaYAMLInts(); c > 0 {
			info.CPU, info.MemoryGB, info.DiskGB = c, m, d
		}
	case "windows":
		info.Engine = "WSL2 / Docker Desktop"
	default:
		info.Engine = "native docker"
		info.Configurable = false
	}
	return info
}

func updateEnvFile(path string, updates map[string]string) {
	raw, _ := os.ReadFile(path)
	lines := strings.Split(string(raw), "\n")
	seen := make(map[string]bool)
	for i, line := range lines {
		for k, v := range updates {
			if strings.HasPrefix(line, k+"=") {
				lines[i] = k + "=" + v
				seen[k] = true
			}
		}
	}
	for k, v := range updates {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

func handleVMStatus(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(getVMInfo())
}

func handleVMApply(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		CPU      int `json:"cpu"`
		MemoryGB int `json:"memory_gb"`
		DiskGB   int `json:"disk_gb"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cur := getVMInfo()
	if req.CPU < 1 || req.CPU > cur.HostCPUs {
		http.Error(w, fmt.Sprintf("CPU 核数须在 1 ~ %d 之间", cur.HostCPUs), http.StatusBadRequest)
		return
	}
	maxMem := cur.HostRAMGB - 4
	if maxMem < 4 {
		maxMem = 64
	}
	if req.MemoryGB < 2 || req.MemoryGB > maxMem {
		http.Error(w, fmt.Sprintf("内存须在 2 ~ %d GB 之间", maxMem), http.StatusBadRequest)
		return
	}
	if req.DiskGB < cur.DiskGB {
		http.Error(w, fmt.Sprintf("虚拟磁盘仅支持扩容，不能小于当前 %d GB", cur.DiskGB), http.StatusBadRequest)
		return
	}

	updateEnvFile(".env", map[string]string{
		"VM_CPU":       strconv.Itoa(req.CPU),
		"VM_MEMORY_GB": strconv.Itoa(req.MemoryGB),
		"VM_DISK_GB":   strconv.Itoa(req.DiskGB),
	})

	// Run VM reconfiguration asynchronously so HTTP response returns cleanly before Docker restarts.
	go func(cpu, mem, disk int) {
		switch runtime.GOOS {
		case "darwin":
			_ = exec.Command("colima", "stop").Run()
			_ = exec.Command("colima", "start", "--vm-type", "vz",
				"--cpu", strconv.Itoa(cpu),
				"--memory", strconv.Itoa(mem),
				"--disk", strconv.Itoa(disk)).Run()
		case "windows":
			if home, err := os.UserHomeDir(); err == nil {
				wslCfg := filepath.Join(home, ".wslconfig")
				content := fmt.Sprintf("[wsl2]\nprocessors=%d\nmemory=%dGB\n", cpu, mem)
				_ = os.WriteFile(wslCfg, []byte(content), 0o600)
			}
			_ = exec.Command("wsl", "--shutdown").Run()
		}
	}(req.CPU, req.MemoryGB, req.DiskGB)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("已写入配置（%d 核 / %d GB / %d GB）并开始重启虚拟机，约 30 秒后自动恢复。", req.CPU, req.MemoryGB, req.DiskGB),
	})
}

func handleVMRestart(w http.ResponseWriter, r *http.Request, cfg *hostConfig) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !checkToken(r, cfg.Token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	go func() {
		switch runtime.GOOS {
		case "darwin":
			_ = exec.Command("colima", "restart").Run()
		case "windows":
			_ = exec.Command("wsl", "--shutdown").Run()
		}
	}()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "正在重启虚拟机，约 20~30 秒后全部容器自动恢复。",
	})
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && v > 0 {
		return v
	}
	return def
}
