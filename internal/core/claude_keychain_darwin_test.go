//go:build darwin

package core

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Round-trips a throwaway login-keychain item through /usr/bin/security. Opt-in,
// because CI runners may have no unlocked login keychain.
func TestClaudeKeychainRoundTrip(t *testing.T) {
	if os.Getenv("AIU_KEYCHAIN_INTEGRATION") != "1" {
		t.Skip("set AIU_KEYCHAIN_INTEGRATION=1 to write a throwaway Keychain item")
	}
	service := fmt.Sprintf("aiu-test-claude-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command(securityBin, "delete-generic-password", "-s", service).Run() })

	if _, _, ok, err := readClaudeKeychain(service); ok || err != nil {
		t.Fatalf("missing item: ok=%v err=%v", ok, err)
	}
	// Larger than `security -i` accepts, with non-ASCII so `-w` answers in hex.
	secret := `{"claudeAiOauth":{"accessToken":"` + strings.Repeat("x", 8000) + `"},"note":"café"}`
	for _, value := range []string{secret, strings.Replace(secret, "café", "thé", 1)} {
		if err := writeClaudeKeychain(service, "example.user", value); err != nil {
			t.Fatal(err)
		}
		got, account, ok, err := readClaudeKeychain(service)
		if err != nil || !ok || account != "example.user" || got != value {
			t.Fatalf("read back ok=%v account=%q err=%v len=%d want %d", ok, account, err, len(got), len(value))
		}
	}
}

// Checks the prompt-free access probes against items created two ways: by
// /usr/bin/security (as Claude Code creates its login) and by this test binary
// through Security.framework (as older AIU releases could).
func TestKeychainAccessProbes(t *testing.T) {
	if os.Getenv("AIU_KEYCHAIN_INTEGRATION") != "1" {
		t.Skip("set AIU_KEYCHAIN_INTEGRATION=1 to write throwaway Keychain items")
	}
	stamp := time.Now().UnixNano()
	bySecurity, byFramework := fmt.Sprintf("aiu-test-acl-cli-%d", stamp), fmt.Sprintf("aiu-test-acl-fw-%d", stamp)
	t.Cleanup(func() {
		_ = exec.Command(securityBin, "delete-generic-password", "-s", bySecurity).Run()
		_ = exec.Command(securityBin, "delete-generic-password", "-s", byFramework).Run()
	})
	if err := writeClaudeKeychain(bySecurity, "example.user", `{"k":1}`); err != nil {
		t.Fatal(err)
	}
	if err := keychainWrite(byFramework, "example.user", `{"k":1}`); err != nil {
		t.Skipf("cgo Keychain unavailable: %v", err)
	}
	for service, want := range map[string]bool{bySecurity: true, byFramework: false} {
		found, trusted, err := claudeItemTrustsSecurity(service)
		if err != nil || !found || trusted != want {
			t.Fatalf("%s: found=%v trusted=%v err=%v, want trusted=%v", service, found, trusted, err, want)
		}
	}
	if found, _, err := claudeItemTrustsSecurity(bySecurity + "-absent"); found || err != nil {
		t.Fatalf("absent item: found=%v err=%v", found, err)
	}
	for service, want := range map[string]string{byFramework: AccessGranted, bySecurity: AccessNeedsApproval, bySecurity + "-absent": AccessMissing} {
		if got, _ := silentItemAccess(service, "example.user"); got != want {
			t.Fatalf("%s: silent access %s, want %s", service, got, want)
		}
	}
}

// Claude Code reads only the item filed under currentUser(); when another item
// shares the service, AIU must read the same one Claude Code does.
func TestReadClaudeKeychainPrefersClaudeCodesAccount(t *testing.T) {
	if os.Getenv("AIU_KEYCHAIN_INTEGRATION") != "1" {
		t.Skip("set AIU_KEYCHAIN_INTEGRATION=1 to write throwaway Keychain items")
	}
	service := fmt.Sprintf("aiu-test-claude-acct-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for range 2 {
			_ = exec.Command(securityBin, "delete-generic-password", "-s", service).Run()
		}
	})
	t.Setenv("USER", "example.user")
	if err := writeClaudeKeychain(service, "someone.else", `{"who":"other"}`); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeKeychain(service, "example.user", `{"who":"claude"}`); err != nil {
		t.Fatal(err)
	}
	got, account, ok, err := readClaudeKeychain(service)
	if err != nil || !ok || account != "example.user" || got != `{"who":"claude"}` {
		t.Fatalf("read %q from %q ok=%v err=%v", got, account, ok, err)
	}
	if found, trusted, err := claudeItemTrustsSecurity(service); err != nil || !found || !trusted {
		t.Fatalf("found=%v trusted=%v err=%v", found, trusted, err)
	}
}
