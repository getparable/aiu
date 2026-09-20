package core

import (
	"os"
	"path/filepath"
	"runtime"
)

// Keep every existing installation where it is. In particular macOS continues
// to use ~/.config/aiu, independently of XDG_CONFIG_HOME. AIU_CONFIG_DIR is
// resolved by DefaultConfig before this fallback is used.
func defaultConfigDir(home string) string {
	configHome, _ := os.UserConfigDir()
	return platformConfigDir(runtime.GOOS, home, configHome, os.Getenv("XDG_CONFIG_HOME"))
}

func platformConfigDir(goos, home, configHome, xdg string) string {
	legacy := filepath.Join(home, ".config", "aiu")
	if goos == "darwin" {
		return legacy
	}
	// An unreadable existing path is still an existing installation: do not
	// silently select an empty store because a permission check failed.
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		return legacy
	}
	if goos == "windows" && filepath.IsAbs(configHome) {
		return filepath.Join(configHome, "aiu")
	}
	if goos == "linux" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "aiu")
	}
	return legacy
}
