//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func linkDir() string                        { h, _ := os.UserHomeDir(); return filepath.Join(h, ".local", "bin") }
func linkPath() string                       { return filepath.Join(linkDir(), "aiu") }
func resolveBinary(e string) (string, error) { return filepath.EvalSymlinks(e) }
func isOwnedLink(i os.FileInfo) bool         { return i.Mode()&os.ModeSymlink != 0 }
func linkTarget() string {
	t, _ := os.Readlink(linkPath())
	if r, e := filepath.EvalSymlinks(linkPath()); e == nil {
		return r
	}
	return t
}
func dirOnLoginPath(d string) bool {
	s := os.Getenv("SHELL")
	if s == "" {
		s = "/bin/zsh"
	}
	o, e := exec.Command(s, "-lic", `printf %s "$PATH"`).Output()
	if e != nil {
		o = []byte(os.Getenv("PATH"))
	}
	for _, p := range strings.Split(string(o), ":") {
		if strings.TrimSpace(p) == d {
			return true
		}
	}
	return false
}
func installLink() error {
	b, e := currentBinary()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(linkDir(), 0755); e != nil {
		return e
	}
	if i, e := os.Lstat(linkPath()); e == nil {
		if !isOwnedLink(i) {
			return fmt.Errorf("%s exists and is not a link — remove it yourself first", linkPath())
		}
		if e = os.Remove(linkPath()); e != nil {
			return e
		}
	}
	return os.Symlink(b, linkPath())
}
func removeLink() error {
	i, e := os.Lstat(linkPath())
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !isOwnedLink(i) {
		return fmt.Errorf("%s is not a link — leaving it alone", linkPath())
	}
	return os.Remove(linkPath())
}

func sameInstallation(target, binary string) bool { return target == binary }
