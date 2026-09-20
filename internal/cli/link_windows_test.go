//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsLinkLifecycleAndOwnership(t *testing.T) {
	if os.Getenv("AIU_LINK_HELPER") == "1" {
		if err := installLink(); err != nil {
			t.Fatal(err)
		}
		if state := readLinkState(); state.State != "installed" {
			t.Fatalf("installed copy: %+v", state)
		}
		return
	}
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	link := linkPath()
	marker := ownerPath()
	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	if s := readLinkState(); s.State != "installed" {
		t.Fatalf("state after install: %+v", s)
	}
	first, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(link)
	if string(first) != string(second) {
		t.Fatal("reinstall changed identical installed bytes")
	}
	if err := removeLink(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(link); !os.IsNotExist(err) {
		t.Fatalf("link remains: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}

	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("unrelated replacement"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installLink(); err == nil {
		t.Fatal("must reject replacement with stale hash")
	}
	if err := removeLink(); err == nil {
		t.Fatal("must refuse removing replacement")
	}
	got, _ := os.ReadFile(link)
	if string(got) != "unrelated replacement" {
		t.Fatal("unrelated replacement was modified")
	}
	_ = os.Remove(link)
	_ = os.Remove(marker)
}

func TestWindowsLinkRejectsInvalidMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(linkDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkPath(), []byte("someone else's exe"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"{}", `{"binary":"x","path":"` + linkPath() + `"}`} {
		if err := os.WriteFile(ownerPath(), []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		if err := installLink(); err == nil {
			t.Fatalf("marker %q authorized overwrite", marker)
		}
	}
}

func TestWindowsLinkSelfInvokedInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, linkPath(), "-test.run=^TestWindowsLinkLifecycleAndOwnership$")
	cmd.Env = append(os.Environ(), "AIU_LINK_HELPER=1", "USERPROFILE="+home, "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installed helper: %v (%s)", err, out)
	}
}

func TestWindowsPathDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	d := filepath.Join(home, ".local", "bin")
	t.Setenv("PATH", `C:\Tools;"`+d+`";C:\Other`)
	if !dirOnLoginPath(d) {
		t.Fatalf("quoted, case-sensitive PATH entry not detected")
	}
	t.Setenv("PATH", `C:\Tools;"`+strings.ToUpper(d)+`";C:\Other`)
	if !dirOnLoginPath(d) {
		t.Fatalf("case-insensitive PATH entry not detected")
	}
	if dirOnLoginPath(filepath.Join(home, "missing")) {
		t.Fatal("missing PATH entry reported present")
	}
}

func TestWindowsMarkerJSONShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := installLink(); err != nil {
		t.Fatal(err)
	}
	var marker linkOwner
	b, _ := os.ReadFile(ownerPath())
	if err := json.Unmarshal(b, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Binary == "" || marker.Path == "" || strings.TrimSpace(marker.Hash) == "" {
		t.Fatal("incomplete ownership marker")
	}
}
