//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

func configureDesktopLaunch(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
