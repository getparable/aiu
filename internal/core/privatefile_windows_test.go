//go:build windows

package core

import (
	"bytes"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func TestReplacementFailureLeavesOriginalAndCleansTemp(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "external.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := writePrivateFile(path, []byte("new"), false); err == nil {
		t.Fatal("replacement unexpectedly succeeded while delete sharing denied")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, []byte("old")) {
		t.Fatalf("original changed: %q", got)
	}
	entries, _ := os.ReadDir(d)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestReplacementPreservesProtectedExternalACL(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "external.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	userSID := tokenUser.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + userSID + ")(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// Windows may recompute SE_DACL_AUTO_INHERITED when creating a new file;
	// compare the actual ACEs, and separately require the protected DACL.
	beforeACEs := before.String()[strings.Index(before.String(), "("):]
	afterACEs := after.String()[strings.Index(after.String(), "("):]
	if beforeACEs != afterACEs {
		t.Fatalf("replacement changed external ACL: before=%s after=%s", before.String(), after.String())
	}
	control, _, err := after.Control()
	if err != nil {
		t.Fatal(err)
	}
	actualACL, _, err := after.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var firstACE *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(actualACL, 0, &firstACE); err != nil {
		t.Fatal(err)
	}
	actualSID := (*windows.SID)(unsafe.Pointer(&firstACE.SidStart))
	if control&windows.SE_DACL_PROTECTED == 0 || !actualSID.Equals(tokenUser.User.Sid) {
		t.Fatalf("replacement ACL is not protected for current user: %s", after.String())
	}
}
