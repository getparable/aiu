package core

import (
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Claude Code reads and writes its Keychain login through /usr/bin/security.
// AIU goes through the same tool, so the item only ever has to trust Apple's
// partition. Security.framework would add AIU's signer as a second partition
// that every Claude Code or AIU access has to win back with a password prompt.

// keychainBlobAttr returns a blob attribute from `security find-generic-password`
// output, which prints it quoted, or as 0x-hex when it holds unprintable bytes.
func keychainBlobAttr(out, name string) (string, bool) {
	prefix := `"` + name + `"<blob>=`
	for _, line := range strings.Split(out, "\n") {
		v, found := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !found {
			continue
		}
		switch {
		case v == "<NULL>":
			return "", true
		case strings.HasPrefix(v, "0x"):
			digits, _, _ := strings.Cut(v[2:], " ")
			b, err := hex.DecodeString(digits)
			if err != nil {
				return "", false
			}
			return string(b), true
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			return v[1 : len(v)-1], true
		}
		return "", false
	}
	return "", false
}

// decodeSecurityPassword undoes `security -w`, which prints a secret as bare hex
// when it holds bytes it will not print raw (for example non-ASCII JSON).
func decodeSecurityPassword(out string) string {
	raw := strings.TrimSuffix(out, "\n")
	if raw == "" || len(raw)%2 != 0 {
		return raw
	}
	b, err := hex.DecodeString(raw)
	if err != nil || !utf8.Valid(b) {
		return raw
	}
	return string(b)
}
