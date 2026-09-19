package core

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/width"
)

// Anything token-shaped must never survive into a message a user might paste into an
// issue: sk-ant-… credentials, bare OAuth grants, Bearer headers and JWTs.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9._-]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9._-]{16,}`),
	regexp.MustCompile(`\brt\.[0-9]+\.[A-Za-z0-9._~+/-]{8,}`),
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._~+/-]{8,}=*`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9._-]{10,}`),
	regexp.MustCompile(`(?i)\b(?:access|refresh|id)[_-]?token"?\s*[:=]\s*"?[A-Za-z0-9._~+/-]{8,}=*`),
	regexp.MustCompile(`(?i)\bcode_verifier"?\s*[:=]\s*"?[A-Za-z0-9._~-]{8,}`),
}

// Redact strips credential-shaped substrings from text headed for a log or terminal.
func Redact(text string) string {
	for _, re := range secretPatterns {
		text = re.ReplaceAllString(text, "[redacted]")
	}
	return text
}

// DisplayWidth is the number of terminal columns text occupies: wide East Asian
// characters and emoji take two, combining marks none.
func DisplayWidth(text string) int {
	n := 0
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		case r >= 0x1F300 && r <= 0x1FAFF:
			n += 2
		default:
			switch width.LookupRune(r).Kind() {
			case width.EastAsianWide, width.EastAsianFullwidth:
				n += 2
			default:
				n++
			}
		}
	}
	return n
}

// writePrivateFile writes owner-only and atomically. ownDir tightens the directory
// too; another tool's directory (~/.claude, ~/.codex) keeps its own mode.
func writePrivateFile(path string, data []byte, ownDir bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if ownDir {
		_ = os.Chmod(dir, 0o700)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writePrivateJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'), true)
}

// readJSONFile decodes path into v; a missing file is ok=false with no error.
func readJSONFile(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// jwtClaims decodes a JWT payload without verifying it — only ever used to read the
// identity and expiry of a token we were just handed.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func jwtExpiryMs(token string) int64 {
	if exp, ok := jwtClaims(token)["exp"].(float64); ok && exp > 0 {
		return int64(exp * 1000)
	}
	return 0
}

// FormatRelative renders a duration the way the table shows reset times.
func FormatRelative(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	minutes := int((d + 30*time.Second) / time.Minute)
	days, hours, mins := minutes/1440, (minutes%1440)/60, minutes%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// FormatLocal renders a wall-clock time compactly, e.g. "Mon 09/21 08:00".
func FormatLocal(t time.Time) string {
	return t.Local().Round(time.Minute).Format("Mon 01/02 15:04")
}

func msToTime(ms int64) time.Time { return time.UnixMilli(ms) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
