package core

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConfigPathsPreserveExistingInstallations(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, ".config", "aiu")
	xdg, roaming := filepath.Join(home, "xdg"), filepath.Join(home, "roaming")
	for _, tc := range []struct{ goos, xdg, want string }{
		{"darwin", xdg, legacy},
		{"linux", "", legacy},
		{"linux", "relative", legacy},
		{"linux", xdg, filepath.Join(xdg, "aiu")},
		{"windows", xdg, filepath.Join(roaming, "aiu")},
	} {
		if got := platformConfigDir(tc.goos, home, roaming, tc.xdg); got != tc.want {
			t.Fatalf("%s (%q): %s, want %s", tc.goos, tc.xdg, got, tc.want)
		}
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"darwin", "windows", "linux"} {
		if got := platformConfigDir(goos, home, roaming, xdg); got != legacy {
			t.Fatalf("%s moved an existing store to %s", goos, got)
		}
	}
}

func TestDefaultConfigExplicitDirectoryAndStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "explicit")
	t.Setenv("AIU_CONFIG_DIR", dir)
	t.Setenv("AIU_STORE", "")
	c := DefaultConfig()
	if c.Dir != dir || c.UseKeychain != (runtime.GOOS == "darwin") || c.UseDPAPI != (runtime.GOOS == "windows") {
		t.Fatal("default platform store or explicit directory was not respected")
	}
	t.Setenv("AIU_STORE", "file")
	c = DefaultConfig()
	if c.Dir != dir || c.UseKeychain || c.UseDPAPI {
		t.Fatal("explicit file storage was not respected")
	}
}
