//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

func configureCmdGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateCmdGroup sends SIGTERM to the entire process group so git/cargo can release locks cleanly,
// then escalates to SIGKILL after 2 seconds to guarantee zero orphan processes.
func terminateCmdGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(2*time.Second, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}
