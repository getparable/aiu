//go:build !windows

package core

import "os/exec"

func openBrowser(rawURL string) bool {
	bin := "xdg-open"
	if isDarwin() {
		bin = "/usr/bin/open"
	}
	cmd := exec.Command(bin, rawURL)
	if err := cmd.Start(); err != nil {
		return false
	}
	// Wait after Start so short-lived launchers do not remain as zombies.
	go func() { _ = cmd.Wait() }()
	return true
}
