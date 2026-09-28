package core

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Keychain access states. Only needs-approval would put a macOS prompt in
// front of the user; the rest need nothing from them.
const (
	AccessGranted       = "granted"        // reads without asking
	AccessNeedsApproval = "needs-approval" // macOS would ask
	AccessMissing       = "missing"        // no item yet, so nothing to approve
	AccessNotUsed       = "not-used"       // this login is not kept in the Keychain
	AccessUnknown       = "unknown"        // could not tell without asking
)

// KeychainAccess is whether one of the Keychain items AIU uses can be read
// without a macOS prompt. ID is "claude" (Claude Code's login) or "aiu" (AIU's
// own token vault).
type KeychainAccess struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// KeychainAccess reports each item without ever showing a prompt, so the
// desktop app can explain an approval before macOS asks for it.
func (c *Config) KeychainAccess() []KeychainAccess {
	return []KeychainAccess{c.claudeKeychainAccess(), c.vaultKeychainAccess()}
}

func (c *Config) claudeKeychainAccess() KeychainAccess {
	a := KeychainAccess{ID: "claude"}
	if l, _ := c.readClaudeFile(); l != nil || runtime.GOOS != "darwin" {
		a.State, a.Detail = AccessNotUsed, "Claude Code keeps its login in a file"
		return a
	}
	found, trusted, err := claudeItemTrustsSecurity(c.ClaudeService)
	switch {
	case err != nil:
		a.State, a.Detail = AccessUnknown, err.Error()
	case !found:
		a.State, a.Detail = AccessMissing, "Claude Code is not signed in"
	case trusted:
		a.State = AccessGranted
	default:
		a.State, a.Detail = AccessNeedsApproval, "macOS will ask before /usr/bin/security can read Claude Code's login"
	}
	return a
}

func (c *Config) vaultKeychainAccess() KeychainAccess {
	a := KeychainAccess{ID: "aiu"}
	if !c.UseKeychain {
		a.State, a.Detail = AccessNotUsed, "AIU does not keep tokens in the Keychain here"
		return a
	}
	a.State, a.Detail = silentItemAccess(c.StoreService, keychainVaultAccount)
	if a.State == AccessMissing {
		a.Detail = "created when you add your first account"
	}
	return a
}

// AllowKeychain reads one item the way AIU normally does, which is what makes
// macOS ask. Choosing Always Allow there is the approval; the read discards
// what it gets.
func (c *Config) AllowKeychain(id string) error {
	switch id {
	case "claude":
		if runtime.GOOS != "darwin" {
			return nil
		}
		_, _, _, err := readClaudeKeychain(c.ClaudeService)
		return err
	case "aiu":
		if !c.UseKeychain {
			return nil
		}
		_, _, err := c.ownKeychainRead(keychainVaultAccount)
		return err
	}
	return errors.New(`keychain item must be "claude" or "aiu"`)
}

// readClaudeFile is readClaudeCode's file branch alone, so a status check never
// falls through to a Keychain read.
func (c *Config) readClaudeFile() (*LiveClaude, error) {
	data, err := os.ReadFile(filepath.Join(c.ClaudeDir, ".credentials.json"))
	if err != nil {
		return nil, err
	}
	return parseLiveClaude(data), nil
}

// partitionsAllowAppleTools reads a partition_id ACL entry, whose description is
// a hex-encoded plist listing partitions such as "apple-tool:" (Apple's command
// line tools, /usr/bin/security among them) or "teamid:…".
func partitionsAllowAppleTools(description string) bool {
	plist, err := hex.DecodeString(description)
	if err != nil {
		plist = []byte(description)
	}
	return strings.Contains(string(plist), "<string>apple-tool:</string>")
}
