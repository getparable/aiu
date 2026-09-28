package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// AIU must speak Claude Code's proper-lockfile protocol: the same two lock
// directories, released afterwards, never stealing a live lock.
func TestWithClaudeRefreshLockUsesClaudeCodeLocks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	locks := []string{filepath.Join(dir, ".oauth_refresh.lock"), real + ".lock"}
	c := &Config{ClaudeDir: dir}
	ran := false
	if err := c.withClaudeRefreshLock(func() error {
		ran = true
		for _, path := range locks {
			if st, err := os.Stat(path); err != nil || !st.IsDir() {
				t.Errorf("%s not held: %v", path, err)
			}
		}
		return nil
	}); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	for _, path := range locks {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", path, err)
		}
	}

	missing := &Config{ClaudeDir: filepath.Join(t.TempDir(), "absent")}
	if err := missing.withClaudeRefreshLock(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing.ClaudeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("locking created a Claude Code directory")
	}
}

func TestAcquireDirLockRespectsLiveAndStaleLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".oauth_refresh.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := acquireDirLock(path, time.Now().Add(150*time.Millisecond)); !errors.Is(err, errClaudeRefreshing) {
		t.Fatalf("live lock: %v", err)
	}
	old := time.Now().Add(-2 * claudeLockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := acquireDirLock(path, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	if st, err := os.Stat(path); err != nil || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("taken-over lock not refreshed: %v", err)
	}
}

// Only the token Claude Code holds can race it. Refreshing any other tracked
// Claude account must not wait on, or hold up, Claude Code's refresh lock.
func TestEnsureFreshLocksOnlyClaudeCodesToken(t *testing.T) {
	api := newFakeAPI(t)
	api.refresh["rt-other"] = "at-other-next"
	c := testConfig(t, api)
	if err := os.MkdirAll(c.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(c.ClaudeDir, ".oauth_refresh.lock")
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	live := &LiveClaude{OAuth: map[string]any{"accessToken": "at-live", "refreshToken": "rt-live"}}
	r := &Record{Provider: Claude, Email: "other@example.test", AccessToken: "at-other", RefreshToken: "rt-other"}
	start := time.Now()
	next, err := c.ensureFresh(t.Context(), r, live, nil, true)
	if err != nil || next.AccessToken != "at-other-next" {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	if waited := time.Since(start); waited > lockWait/2 {
		t.Fatalf("waited %s on Claude Code's lock for a token it does not hold", waited)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("Claude Code's live lock was disturbed: %v", err)
	}
}
