//go:build !windows && !darwin

package cli

import (
	"os/exec"
	"syscall"
)

func configureDesktopLaunch(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
