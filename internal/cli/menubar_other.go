//go:build !darwin

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func openMenuBar() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Resolve an installed Unix CLI symlink before looking for its bundled UI.
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	name := "aiu-desktop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(filepath.Dir(self), name)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return fmt.Errorf("AIU desktop is not bundled beside this CLI; download the Windows/Linux desktop archive, or use `aiu watch`")
	}
	cmd := exec.Command(path)
	configureDesktopLaunch(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// The independently launched desktop owns its Go children. It must survive
	// the short-lived launcher, and this process retains no child pipe handles.
	return cmd.Process.Release()
}
