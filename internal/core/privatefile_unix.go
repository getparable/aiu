//go:build !windows

package core

import (
	"os"
	"path/filepath"
)

func createPrivateTemp(path string, ownDir, preserveMode bool) (*os.File, error) {
	mode := os.FileMode(0o600)
	if preserveMode {
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

func securePrivateDir(path string) error        { return os.Chmod(path, 0o700) }
func replacePrivateFile(tmp, path string) error { return os.Rename(tmp, path) }
