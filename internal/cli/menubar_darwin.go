//go:build darwin

package cli

import (
	"errors"
	"os/exec"
)

func openMenuBar() error {
	if err := exec.Command("/usr/bin/open", "-a", "AIU").Run(); err != nil {
		return errors.New("AIU.app is not installed — run `make install` in the aiu repo")
	}
	return nil
}
