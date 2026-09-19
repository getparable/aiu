package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkInstallRemoveAndRefuseRealFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	link := filepath.Join(home, ".local", "bin", "aiu")

	if got := readLinkState().State; got != "missing" {
		t.Fatalf("state = %q, want missing", got)
	}
	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	state := readLinkState()
	binary, _ := currentBinary()
	if state.State != "installed" || state.Path != link || state.Target != binary {
		t.Fatalf("after install: %+v (binary %s)", state, binary)
	}
	// Installing twice replaces our own link rather than failing.
	if err := installLink(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if err := removeLink(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("remove left the link behind")
	}
	// Removing what is not there is not an error.
	if err := removeLink(); err != nil {
		t.Fatalf("second remove: %v", err)
	}

	// Someone else's real file at that path is never touched.
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installLink(); err == nil {
		t.Fatal("install must refuse to replace a real file")
	}
	if err := removeLink(); err == nil {
		t.Fatal("remove must refuse to delete a real file")
	}
	if data, err := os.ReadFile(link); err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("the real file was modified: %q %v", data, err)
	}
	if got := readLinkState().State; got != "occupied" {
		t.Fatalf("state = %q, want occupied", got)
	}
}
