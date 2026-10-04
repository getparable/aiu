package core

import (
	"os/exec"
	"syscall"
)

// detachFromTerminalSignals disables Ctrl+C for the daemon lifecycle command,
// which a new process group does not inherit from the console.
func detachFromTerminalSignals(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
