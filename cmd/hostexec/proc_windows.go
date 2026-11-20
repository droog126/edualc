//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func configureCmdGroup(cmd *exec.Cmd) {}

func terminateCmdGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Kill entire process tree on Windows so child rustc/cargo processes never linger.
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
