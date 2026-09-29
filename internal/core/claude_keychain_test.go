package core

import (
	"encoding/hex"
	"fmt"
	"testing"
)

func TestKeychainBlobAttr(t *testing.T) {
	out := `keychain: "/Users/example/Library/Keychains/login.keychain-db"
attributes:
    "acct"<blob>="example.user"
    "gena"<blob>=<NULL>
    "icmt"<blob>=0x636166C3A9  "caf\303\251"
    "svce"<blob>="Claude Code-credentials"
`
	for name, want := range map[string]string{"acct": "example.user", "gena": "", "icmt": "café", "svce": "Claude Code-credentials"} {
		if got, ok := keychainBlobAttr(out, name); !ok || got != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, ok, want)
		}
	}
	if _, ok := keychainBlobAttr(out, "labl"); ok {
		t.Fatal("absent attribute reported present")
	}
}

func TestDecodeSecurityPassword(t *testing.T) {
	for raw, want := range map[string]string{
		`{"claudeAiOauth":{}}` + "\n": `{"claudeAiOauth":{}}`,
		"7b22e284a2223a317d\n":        `{"™":1}`,
		"abc\n":                       "abc",
		"":                            "",
	} {
		if got := decodeSecurityPassword(raw); got != want {
			t.Fatalf("decodeSecurityPassword(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCurrentUserMatchesClaudeCode(t *testing.T) {
	for user, want := range map[string]string{"example.user": "example.user", "a b": "claude-code-user", "ünï": "claude-code-user"} {
		t.Setenv("USER", user)
		if got := currentUser(); got != want {
			t.Fatalf("USER=%q gave %q, want %q", user, got, want)
		}
	}
}

func TestPartitionsAllowAppleTools(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Partitions</key><array><string>%s</string></array></dict></plist>`
	for partition, want := range map[string]bool{"apple-tool:": true, "teamid:RB2G649FSL": false, "apple:": false} {
		raw := fmt.Sprintf(plist, partition)
		if got := partitionsAllowAppleTools(hex.EncodeToString([]byte(raw))); got != want {
			t.Fatalf("%s: got %v", partition, got)
		}
	}
}
