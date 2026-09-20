package core

import "testing"

func TestOpenBrowserRejectsUnsafeURLs(t *testing.T) {
	c := DefaultConfig()
	for _, raw := range []string{
		`https://user:pass@claude.ai/oauth/authorize`,
		"https://claude.ai/oauth/authorize\n--bad",
		`https://claude.ai/oauth/authorize" --bad`,
		"http://claude.ai/oauth/authorize",
		"https://evil.example/oauth/authorize",
	} {
		if c.OpenBrowser(raw) {
			t.Fatalf("unsafe URL was accepted: %q", raw)
		}
	}
}
