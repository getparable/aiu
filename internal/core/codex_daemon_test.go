package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDaemonConfig installs a fake Codex whose Nth `daemon restart` runs
// restarts[N-1] (the last entry repeats). The daemon PID file starts at 101.
func fakeDaemonConfig(t *testing.T, restarts ...string) *Config {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex executable uses a POSIX shell")
	}
	c := DefaultConfig()
	c.Dir = t.TempDir()
	c.CodexHome = filepath.Join(c.Dir, "codex")
	c.CodexRestartWait = 300 * time.Millisecond
	c.CodexForceWait = time.Second
	daemonDir := filepath.Join(c.CodexHome, "app-server-daemon")
	if err := os.MkdirAll(daemonDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "app-server.pid"), []byte(`{"pid":101,"processStartTime":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := ""
	for i, body := range restarts {
		pattern := string(rune('1' + i))
		if i == len(restarts)-1 {
			pattern = "*"
		}
		cases += pattern + ") " + body + " ;;\n"
	}
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"'app-server daemon version') echo '{\"status\":\"running\"}' ;;\n" +
		"'app-server daemon restart')\n" +
		"  n=$(( $(cat \"$CODEX_HOME/restarts\" 2>/dev/null || echo 0) + 1 )); echo $n > \"$CODEX_HOME/restarts\"\n" +
		"  case $n in\n" + cases + "  esac ;;\n" +
		"*) exit 99 ;;\nesac\n"
	c.CodexBinary = filepath.Join(c.Dir, "fake-codex")
	if err := os.WriteFile(c.CodexBinary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return c
}

// replaced simulates Codex starting a new daemon with the given PID.
func replaced(pid string) string {
	return `echo '{"pid":` + pid + `}' > "$CODEX_HOME/app-server-daemon/app-server.pid"; echo '{"status":"restarted","pid":` + pid + `}'`
}

func restartCount(t *testing.T, c *Config) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(c.CodexHome, "restarts"))
	return strings.TrimSpace(string(data))
}

func TestCodexDaemonRestartOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		restarts []string
		status   string
		calls    string
		forced   bool
		pending  bool
	}{
		{"graceful", []string{replaced("202")}, "restarted", "1", false, false},
		// The first restart drains but outlives its budget; a second restart
		// forces the draining daemon out and starts a replacement.
		{"stuck drain is forced", []string{"exec sleep 30", replaced("303")}, "restarted", "2", true, false},
		{"both attempts hang", []string{"exec sleep 30"}, "restart_failed", "2", false, true},
		{"same daemon survives", []string{`echo '{"status":"restarted","pid":101}'`}, "restart_failed", "2", false, true},
		{"unparseable success", []string{"exit 0"}, "restart_failed", "2", false, true},
		{"failure then recovery", []string{"exit 9", replaced("404")}, "restarted", "2", true, false},
		// The first attempt never got Codex's lifecycle lock; the second gets the
		// longer budget a full graceful drain needs.
		{"late drain on second attempt", []string{"exec sleep 30", "sleep 0.6; " + replaced("505")}, "restarted", "2", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := fakeDaemonConfig(t, tt.restarts...)
			start := time.Now()
			res := c.restartCodexDaemon(context.Background())
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("restart took %v", elapsed)
			}
			if res.Status != tt.status || res.Warning != (tt.status != "restarted") {
				t.Fatalf("result = %+v", res)
			}
			if got := restartCount(t, c); got != tt.calls {
				t.Fatalf("restart attempts = %s, want %s", got, tt.calls)
			}
			if tt.forced != strings.Contains(res.Message, "second attempt") {
				t.Fatalf("forced restart not reported correctly: %q", res.Message)
			}
			if c.codexRestartPending() != tt.pending {
				t.Fatalf("pending marker = %v, want %v", !tt.pending, tt.pending)
			}
		})
	}
}

func TestCodexDaemonRestartOutlivesCallerCancellation(t *testing.T) {
	// Cancelling mid-restart is what left the daemon draining with nothing to
	// replace it, so a started restart must finish on its own budget.
	c := fakeDaemonConfig(t, "sleep 0.3; "+replaced("202"))
	c.CodexRestartWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for restartCount(t, c) == "" {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	if res := c.restartCodexDaemon(ctx); res.Status != "restarted" {
		t.Fatalf("result = %+v", res)
	}
}

func TestCodexDaemonRestartLogsWithoutOutput(t *testing.T) {
	c := fakeDaemonConfig(t, "echo synthetic-secret >&2; exit 9", replaced("202"))
	c.restartCodexDaemon(context.Background())
	data, err := os.ReadFile(c.codexDaemonLog())
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"version", "restart", "exit status 9", "pid 101 -> 202", "restarted"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "synthetic-secret") {
		t.Fatal("log recorded raw Codex output")
	}
}

func TestCodexRestartPendingSurvivesUnfinishedProbe(t *testing.T) {
	// Credentials are saved before the daemon is reconciled, so an interrupted
	// probe must still leave a retry for the next selection.
	c := fakeDaemonConfig(t, replaced("202"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := c.restartCodexDaemon(ctx); res.Status != "unavailable" || !c.codexRestartPending() {
		t.Fatalf("result = %+v, pending = %v", res, c.codexRestartPending())
	}
}

func TestCodexDaemonRestartReportsProgress(t *testing.T) {
	c := fakeDaemonConfig(t, "exec sleep 30", replaced("202"))
	var info []string
	c.Info = func(m string) { info = append(info, m) }
	c.restartCodexDaemon(context.Background())
	got := strings.Join(info, "\n")
	for _, want := range []string{"Restarting the Codex daemon", "still finishing turns"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress missing %q:\n%s", want, got)
		}
	}
}

func TestCodexRestartRetrySkipsReplacedDaemon(t *testing.T) {
	c := fakeDaemonConfig(t, "exec sleep 30")
	if res := c.restartCodexDaemon(context.Background()); res.Status != "restart_failed" {
		t.Fatalf("first result = %+v", res)
	}
	// The user restarted Codex by hand: a different daemon now holds the PID
	// record, so it started after the saved login and needs no restart.
	pidFile := filepath.Join(c.CodexHome, "app-server-daemon", "app-server.pid")
	if err := os.WriteFile(pidFile, []byte(`{"pid":909,"processStartTime":"later"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := c.retryCodexRestart(context.Background()); res.Status != "restarted" || res.Warning {
		t.Fatalf("retry result = %+v", res)
	}
	if got := restartCount(t, c); got != "2" {
		t.Fatalf("replaced daemon was restarted again: %s attempts", got)
	}
	if c.codexRestartPending() {
		t.Fatal("pending marker kept after a replacement daemon was found")
	}
}

func TestCodexRestartRetryRestartsSameDaemon(t *testing.T) {
	// PID reuse: the same PID with a different start time is a new daemon,
	// while an identical record means the stuck daemon is still there.
	c := fakeDaemonConfig(t, "exec sleep 30", "exec sleep 30", replaced("202"))
	c.restartCodexDaemon(context.Background())
	if res := c.retryCodexRestart(context.Background()); res.Status != "restarted" {
		t.Fatalf("retry result = %+v", res)
	}
	if got := restartCount(t, c); got != "3" {
		t.Fatalf("stuck daemon was not restarted: %s attempts", got)
	}
}

func TestCodexRestartPendingClearsOnSuccess(t *testing.T) {
	c := fakeDaemonConfig(t, "exec sleep 30", "exec sleep 30", replaced("202"))
	if res := c.restartCodexDaemon(context.Background()); res.Status != "restart_failed" || !c.codexRestartPending() {
		t.Fatalf("first result = %+v", res)
	}
	if res := c.restartCodexDaemon(context.Background()); res.Status != "restarted" || c.codexRestartPending() {
		t.Fatalf("retry result = %+v, pending = %v", res, c.codexRestartPending())
	}
}
