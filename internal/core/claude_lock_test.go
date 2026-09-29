package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
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
	if _, err := acquireDirLock(path, takeoverPath(t), time.Now().Add(150*time.Millisecond)); !errors.Is(err, errClaudeRefreshing) {
		t.Fatalf("live lock: %v", err)
	}
	old := time.Now().Add(-2 * claudeLockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireDirLock(path, takeoverPath(t), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	t.Cleanup(lock.release)
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

// writeClaudeLogin stands in for Claude Code writing its login file.
func writeClaudeLogin(t *testing.T, c *Config, access, refresh string, expiresAt int64) {
	t.Helper()
	doc := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, access, refresh, expiresAt)
	if err := os.WriteFile(filepath.Join(c.ClaudeDir, ".credentials.json"), []byte(doc), 0o600); err != nil {
		t.Error(err)
	}
}

// holdClaudeLockWhileRefreshing plays Claude Code mid-refresh: it holds the
// refresh lock, writes its replacement login, then releases the lock.
func holdClaudeLockWhileRefreshing(t *testing.T, c *Config, access, refresh string) {
	t.Helper()
	lock := filepath.Join(c.ClaudeDir, ".oauth_refresh.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	expires := c.now().Add(8 * time.Hour).UnixMilli()
	go func() {
		time.Sleep(200 * time.Millisecond)
		writeClaudeLogin(t, c, access, refresh, expires)
		_ = os.Remove(lock)
	}()
}

// Claude Code refreshing while AIU waits for its lock retires the token AIU was
// about to spend; AIU must take the replacement, and only for the same account.
func TestEnsureFreshRereadsClaudeCodeAfterItsLock(t *testing.T) {
	for _, tc := range []struct {
		name, replacementEmail, want string
	}{
		{name: "same account adopts Claude Code's refresh", replacementEmail: "me@example.test", want: "at-B"},
		{name: "another account is never adopted", replacementEmail: "other@example.test", want: "at-A2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.emails["at-B"], api.orgs["at-B"] = tc.replacementEmail, [2]string{"org-me", "Org"}
			api.refresh["rt-A"] = "at-A2" // only reachable if AIU spends the retired token
			c := testConfig(t, api)
			if err := os.MkdirAll(c.ClaudeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			past := c.now().Add(-time.Hour).UnixMilli()
			writeClaudeLogin(t, c, "at-A", "rt-A", past)
			live := c.ReadClaudeCode()
			r := &Record{Provider: Claude, Email: "me@example.test", OrgUUID: "org-me", AccessToken: "at-A", RefreshToken: "rt-A", ExpiresAt: past}
			holdClaudeLockWhileRefreshing(t, c, "at-B", "rt-B")
			next, err := c.ensureFresh(t.Context(), r, live, nil, false)
			if err != nil || next.AccessToken != tc.want {
				t.Fatalf("access=%v err=%v, want %s", next, err, tc.want)
			}
		})
	}
}

// Adding Claude Code's account mid-refresh must add its new login, not spend the
// token Claude Code just retired.
func TestCaptureClaudeCodeRereadsAfterClaudeCodesLock(t *testing.T) {
	api := newFakeAPI(t)
	api.emails["at-B"], api.orgs["at-B"] = "me@example.test", [2]string{"org-me", "Org"}
	api.usage["at-B"] = `{}`
	c := testConfig(t, api)
	if err := os.MkdirAll(c.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeLogin(t, c, "at-A", "rt-A", c.now().Add(-time.Hour).UnixMilli())
	holdClaudeLockWhileRefreshing(t, c, "at-B", "rt-B")
	saved, err := c.CaptureClaudeCode(t.Context(), "")
	if err != nil || saved.Record.AccessToken != "at-B" || saved.Record.RefreshToken != "rt-B" {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

func takeoverPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".aiu-credentials.lock")
}

// A contender that saw a lock stale must not remove the fresh lock another
// contender has since won in its place.
func TestRemoveStaleDirLockSparesAFreshWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".oauth_refresh.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * claudeLockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	seen, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Another contender takes it over first and wins a fresh lock.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleDirLock(path, seen, takeoverPath(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the fresh winner's lock was removed: %v", err)
	}
}

// Many contenders racing to take over one stale lock: exactly one holds it at a time.
func TestAcquireDirLockStaleTakeoverIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path, takeover := filepath.Join(dir, ".oauth_refresh.lock"), filepath.Join(dir, ".aiu-credentials.lock")
	for round := range 10 {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * claudeLockStale)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		var holders, peak atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lock, err := acquireDirLock(path, takeover, time.Now().Add(5*time.Second))
				if err != nil {
					t.Error(err)
					return
				}
				n := holders.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(2 * time.Millisecond)
				holders.Add(-1)
				lock.release()
			}()
		}
		wg.Wait()
		if peak.Load() != 1 {
			t.Fatalf("round %d: %d contenders held the lock at once", round, peak.Load())
		}
	}
}

// skipTakeoverOnWindows skips tests of a lock replaced under its holder. Windows
// cannot remove a directory a process holds open, which is that protection
// itself, so the scenario cannot arise there. It runs before any lock is taken:
// a skip with a lock still open would fail TempDir cleanup on Windows.
func skipTakeoverOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to remove a lock directory its holder keeps open")
	}
}

// replaceDirLock plays Claude Code taking over a lock that went stale while its
// holder was paused.
func replaceDirLock(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// After a pause long enough for Claude Code to take a lock over, AIU's heartbeat
// and release must leave Claude Code's replacement lock untouched.
func TestHeldDirLockLeavesAReplacementAlone(t *testing.T) {
	skipTakeoverOnWindows(t)
	path := filepath.Join(t.TempDir(), ".oauth_refresh.lock")
	lock, err := acquireDirLock(path, takeoverPath(t), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	theirs := replaceDirLock(t, path)
	lock.touch(time.Now().Add(time.Hour))
	lock.release()
	now, err := os.Stat(path)
	if err != nil {
		t.Fatalf("release removed Claude Code's replacement lock: %v", err)
	}
	if !now.ModTime().Equal(theirs.ModTime()) {
		t.Fatalf("heartbeat touched Claude Code's replacement lock: %v -> %v", theirs.ModTime(), now.ModTime())
	}

	// An owned lock is still kept fresh and released.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = lock.dir.Close()
	lock, err = acquireDirLock(path, takeoverPath(t), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(time.Minute)
	lock.touch(stamp)
	if now, err := os.Stat(path); err != nil || !now.ModTime().Equal(stamp) {
		t.Fatalf("owned lock not touched: %v %v", now, err)
	}
	lock.release()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned lock not released: %v", err)
	}
}

// Releasing withClaudeRefreshLock after a takeover mid-callback must not delete
// the lock Claude Code now holds.
func TestWithClaudeRefreshLockSparesATakeoverOnRelease(t *testing.T) {
	skipTakeoverOnWindows(t)
	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Config{ClaudeDir: dir, Dir: t.TempDir()}
	path := filepath.Join(dir, ".oauth_refresh.lock")
	if err := c.withClaudeRefreshLock(func() error {
		replaceDirLock(t, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Claude Code's lock was removed on release: %v", err)
	}
}

// A takeover landing between touch's ownership check and its stamp must not make
// AIU adopt Claude Code's replacement, or release would delete it.
func TestHeldDirLockTouchNeverAdoptsAReplacement(t *testing.T) {
	skipTakeoverOnWindows(t)
	path := filepath.Join(t.TempDir(), ".oauth_refresh.lock")
	lock, err := acquireDirLock(path, takeoverPath(t), time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	previous := touchLockPath
	t.Cleanup(func() { touchLockPath = previous })
	touchLockPath = func(name string, atime, mtime time.Time) error {
		replaceDirLock(t, name) // Claude Code takes over right after the check
		return os.Chtimes(name, atime, mtime)
	}
	lock.touch(time.Now().Add(time.Minute))
	if lock.owned() {
		t.Fatal("AIU adopted Claude Code's replacement lock as its own")
	}
	lock.release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release removed Claude Code's replacement lock: %v", err)
	}
}
