package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A Codex-only view must never read Claude Code's Keychain item: that read is
// the prompt users see. The unfiltered control proves the seam is live.
func TestCollectSkipsClaudeLoginOutsideView(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Claude Code keeps its login in the Keychain only on macOS")
	}
	api := newFakeAPI(t)
	c := testConfig(t, api)
	expires := c.now().Add(time.Hour).UnixMilli()
	claude := &Record{Provider: Claude, Email: "a@example.com", OrgUUID: "org-a", AccessToken: "at-claude", ExpiresAt: expires}
	codex := &Record{Provider: Codex, Email: "b@example.com", OrgUUID: "acct-b", AccountID: "acct-b", AccessToken: "at-codex", ExpiresAt: expires}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"accounts.json": &Index{Version: 1, Accounts: []*IndexEntry{
			{Provider: Claude, Email: claude.Email, Org: claude.OrgUUID},
			{Provider: Codex, Email: codex.Email, Org: codex.OrgUUID},
		}},
		"tokens.json": map[string]*Record{claude.StoreKey(): claude, codex.StoreKey(): codex},
	} {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.Dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reads := 0
	previous := claudeKeychainRead
	claudeKeychainRead = func(string) (string, string, bool, error) { reads++; return "", "", false, nil }
	t.Cleanup(func() { claudeKeychainRead = previous })

	ctx := context.Background()
	if _, err := c.Collect(ctx, CollectOptions{Providers: []Provider{Codex}}); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("Codex-only collect read Claude Code's Keychain item %d times", reads)
	}
	if _, err := c.Collect(ctx, CollectOptions{}); err != nil {
		t.Fatal(err)
	}
	if reads == 0 {
		t.Fatal("unfiltered collect never reached the Keychain seam")
	}
}
