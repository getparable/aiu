//go:build windows

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"
)

func TestDPAPIStoreRoundTripAndCiphertext(t *testing.T) {
	d := t.TempDir()
	c := DefaultConfig()
	c.Dir = d
	c.UseDPAPI = true
	want := map[string]*Record{"claude:a@example.test": {Email: "a@example.test", AccessToken: "synthetic-secret"}}
	if err := writeTokenStore(c, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(d, "tokens.dpapi"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("synthetic-secret")) {
		t.Fatal("ciphertext contains plaintext secret")
	}
	var got map[string]*Record
	if ok, err := readTokenStore(c, &got); err != nil || !ok || got["claude:a@example.test"].AccessToken != "synthetic-secret" {
		t.Fatalf("roundtrip: ok=%v err=%v got=%v", ok, err, got)
	}
}

func TestDPAPICorruptLeavesFileUnchanged(t *testing.T) {
	d := t.TempDir()
	c := DefaultConfig()
	c.Dir = d
	c.UseDPAPI = true
	path := filepath.Join(d, "tokens.dpapi")
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	var out map[string]*Record
	if _, err := readTokenStore(c, &out); err == nil {
		t.Fatal("corrupt DPAPI accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("corrupt file changed")
	}
}

func TestDPAPIRefusesExistingPlaintext(t *testing.T) {
	d := t.TempDir()
	c := DefaultConfig()
	c.Dir = d
	c.UseDPAPI = true
	plain := map[string]*Record{"x": {Email: "x@example.test"}}
	b, _ := json.Marshal(plain)
	if err := os.WriteFile(filepath.Join(d, "tokens.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	var out map[string]*Record
	if _, err := readTokenStore(c, &out); err == nil {
		t.Fatal("plaintext store silently hidden")
	}
}

func TestPrivateFileUsesProtectedDACL(t *testing.T) {
	d := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(d, "data.json")
	if err := writePrivateJSON(path, map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL is inheritable: %s", sd.String())
	}
}

func TestDPAPIWriteFailurePreservesCiphertext(t *testing.T) {
	d := t.TempDir()
	c := DefaultConfig()
	c.Dir = d
	c.UseDPAPI = true
	old := map[string]*Record{"old": {AccessToken: "old-secret"}}
	if err := writeTokenStore(c, old); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(d, "tokens.dpapi"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	before, _ := os.ReadFile(filepath.Join(d, "tokens.dpapi"))
	if err := writeTokenStore(c, map[string]*Record{"new": {AccessToken: "new-secret"}}); err == nil {
		t.Fatal("DPAPI replacement unexpectedly succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(d, "tokens.dpapi"))
	if !bytes.Equal(before, after) {
		t.Fatal("ciphertext changed after failed replacement")
	}
}

func TestDPAPIConcurrentProcessWriters(t *testing.T) {
	if dir := os.Getenv("AIU_DPAPI_CHILD"); dir != "" {
		c := &Config{Dir: dir, UseDPAPI: true}
		prefix := os.Getenv("AIU_DPAPI_PREFIX")
		for i := range 8 {
			if err := c.tokenSet(fmt.Sprintf("%s-%d", prefix, i), &Record{AccessToken: "synthetic-" + prefix}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 4)
	for p := range commands {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDPAPIConcurrentProcessWriters$")
		cmd.Env = append(os.Environ(), "AIU_DPAPI_CHILD="+dir, fmt.Sprintf("AIU_DPAPI_PREFIX=%d", p))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands[p] = cmd
		defer cmd.Wait()
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	c := &Config{Dir: dir, UseDPAPI: true}
	for p := range commands {
		for i := range 8 {
			if r, err := c.tokenGet(fmt.Sprintf("%d-%d", p, i)); err != nil || r == nil || r.AccessToken != fmt.Sprintf("synthetic-%d", p) {
				t.Fatalf("lost concurrent record %d-%d (error=%v)", p, i, err)
			}
		}
	}
}

func assertCurrentUserOnlyACL(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("expected one current-user ACE: %s (%v)", sd.String(), err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if control&windows.SE_DACL_PROTECTED == 0 || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
		t.Fatalf("expected only the current user's protected ACE for %s: %s", filepath.Base(path), sd.String())
	}
}

func TestPrivateACLAtCreationAndAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	// This assertion runs before any byte is written to the temporary file.
	tmp, err := createPrivateTemp(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentUserOnlyACL(t, tmp.Name())
	tmp.Close()
	os.Remove(tmp.Name())
	if err := writePrivateJSON(path, map[string]string{"old": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	broad, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := broad.DACL()
	if err != nil {
		t.Fatal(err)
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(path, map[string]string{"new": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	assertCurrentUserOnlyACL(t, path)
	assertCurrentUserOnlyACL(t, dir)
	// Directory inheritance keeps ordinary lock files reopenable by the next process.
	lock := filepath.Join(dir, "test.lock")
	for range 2 {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}
