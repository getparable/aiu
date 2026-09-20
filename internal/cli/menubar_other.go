//go:build !darwin

package cli

import "errors"

func openMenuBar() error {
	return errors.New("the AIU menu bar app is available on macOS only; use `aiu status` or `aiu watch` on this platform")
}
