//go:build !windows

package core

import (
	"os/exec"
	"syscall"
)

// detachFromTerminalSignals keeps Ctrl-C, which reaches the whole foreground
// process group, from killing a daemon lifecycle command mid-restart.
func detachFromTerminalSignals(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
