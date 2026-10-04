package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CodexDaemonResult distinguishes a saved login from a restarted backend.
// A backend failure does not undo a successful credential switch.
type CodexDaemonResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Warning bool   `json:"warning"`
}

const codexResumeGuidance = "For standalone sessions, exit Codex and run `codex resume --last --no-daemon` (omit --no-daemon on older versions)."

// Covers a token refresh (one minute), probing (5s), both restart attempts
// (100s by default), and storage.
const codexSwitchLockWait = 4 * time.Minute

// A graceful restart drains active turns before replacing the daemon. When it
// outlives codexRestartWait, a second restart forces the draining daemon out;
// codexForceWait also covers a full restart if the first never got started.
const (
	codexRestartWait = 10 * time.Second
	codexForceWait   = 90 * time.Second
)

func codexDaemonUnchanged() *CodexDaemonResult {
	return &CodexDaemonResult{Status: "unchanged", Message: "Saved login unchanged; Codex daemon was not restarted. If a connected session still uses another account, run `codex app-server daemon restart`. " + codexResumeGuidance}
}

// restartCodexDaemon restarts a running daemon and records whether a later
// switch must retry it, since a half-finished restart leaves Codex refusing
// new sessions ("Server is draining") until another restart completes.
func (c *Config) restartCodexDaemon(ctx context.Context) *CodexDaemonResult {
	res := c.reconcileCodexDaemon(ctx)
	switch res.Status {
	case "restart_failed":
		c.setCodexRestartPending(true)
	case "restarted", "not_running", "unsupported":
		c.setCodexRestartPending(false)
	}
	c.logCodexDaemon("result " + res.Status)
	return res
}

func (c *Config) reconcileCodexDaemon(ctx context.Context) *CodexDaemonResult {
	binary, err := c.codexExecutable()
	if err != nil {
		return &CodexDaemonResult{Status: "unavailable", Message: "Codex executable unavailable; only the saved login changed. Set AIU_CODEX_BIN to its path to enable daemon restarts. " + codexResumeGuidance, Warning: true}
	}
	output, diagnostic, err := c.runCodexDaemonCommand(ctx, binary, "version", 5*time.Second)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && ctx.Err() == nil && exitErr.Exited() {
			// These are specific CLI diagnostics, not an inference from the exit
			// code. Other failures (including permission errors) need a warning.
			if strings.Contains(diagnostic, "failed to connect to ") && strings.Contains(diagnostic, "app-server-control.sock") && strings.Contains(diagnostic, "No such file or directory (os error 2)") {
				return &CodexDaemonResult{Status: "not_running", Message: "Codex daemon is not running; only the saved login changed. " + codexResumeGuidance}
			}
			if strings.Contains(diagnostic, "unrecognized subcommand 'daemon'") || strings.Contains(diagnostic, "unrecognized subcommand 'app-server'") {
				return &CodexDaemonResult{Status: "unsupported", Message: "This Codex version has no daemon management; only the saved login changed. " + codexResumeGuidance}
			}
		}
		return &CodexDaemonResult{Status: "unavailable", Message: "Login saved, but Codex daemon status could not be checked. Run `codex app-server daemon restart` for connected sessions. " + codexResumeGuidance, Warning: true}
	}
	var version struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(output, &version) != nil || version.Status == "" {
		return &CodexDaemonResult{Status: "unavailable", Message: "Login saved, but Codex returned an unrecognized daemon status. Run `codex app-server daemon restart` for connected sessions. " + codexResumeGuidance, Warning: true}
	}
	if version.Status == "stopped" || version.Status == "not_running" {
		return &CodexDaemonResult{Status: "not_running", Message: "Codex daemon is not running; only the saved login changed. " + codexResumeGuidance}
	}
	if version.Status != "running" {
		return &CodexDaemonResult{Status: "unavailable", Message: "Login saved, but Codex returned an unrecognized daemon status. Run `codex app-server daemon restart` for connected sessions. " + codexResumeGuidance, Warning: true}
	}

	// The restart CLI owns the drain timer, the forced kill and the replacement
	// start. Killing it early on the caller's behalf strands the daemon in
	// drain, so both attempts run on their own budgets.
	ctx = context.WithoutCancel(ctx)
	before := c.codexDaemonPID()
	for attempt, wait := range []time.Duration{durationOr(c.CodexRestartWait, codexRestartWait), durationOr(c.CodexForceWait, codexForceWait)} {
		output, _, err := c.runCodexDaemonCommand(ctx, binary, "restart", wait)
		pid := restartedPID(output)
		replaced := err == nil && pid > 0 && pid != before
		if err == nil {
			c.logCodexDaemon(fmt.Sprintf("restart attempt %d: pid %d -> %d", attempt+1, before, pid))
		}
		if !replaced {
			continue
		}
		if attempt == 0 {
			return &CodexDaemonResult{Status: "restarted", Message: "Codex daemon restarted. Connected terminals will reconnect; current turns were interrupted. " + codexResumeGuidance}
		}
		return &CodexDaemonResult{Status: "restarted", Message: "Codex daemon restarted after the previous daemon was force-stopped. Connected terminals will reconnect; re-send any interrupted turn. " + codexResumeGuidance}
	}
	return &CodexDaemonResult{Status: "restart_failed", Message: "Login saved, but the Codex daemon restart did not complete, so new Codex sessions may fail with \"Server is draining\". Run `codex app-server daemon restart` (running it again forces a stuck daemon to stop); AIU also retries when you select this account again. " + codexResumeGuidance, Warning: true}
}

func durationOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// restartedPID returns the replacement PID from `daemon restart` JSON output.
func restartedPID(output []byte) int {
	var result struct {
		Status string `json:"status"`
		PID    int    `json:"pid"`
	}
	if json.Unmarshal(output, &result) != nil || result.Status != "restarted" {
		return 0
	}
	return result.PID
}

// codexDaemonPID reads the managed daemon's PID record, or 0 when absent.
func (c *Config) codexDaemonPID() int {
	for _, name := range []string{"daemon.pid", "app-server.pid"} {
		data, err := os.ReadFile(filepath.Join(c.CodexHome, "app-server-daemon", name))
		if err != nil {
			continue
		}
		var record struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &record) == nil && record.PID > 0 {
			return record.PID
		}
	}
	return 0
}

func (c *Config) codexRestartPendingFile() string {
	return filepath.Join(c.CodexHome, ".aiu-daemon-restart-pending")
}

func (c *Config) codexRestartPending() bool {
	_, err := os.Stat(c.codexRestartPendingFile())
	return err == nil
}

func (c *Config) setCodexRestartPending(pending bool) {
	if !pending {
		os.Remove(c.codexRestartPendingFile())
		return
	}
	if err := os.WriteFile(c.codexRestartPendingFile(), nil, 0o600); err != nil {
		c.Warn("could not record the unfinished Codex daemon restart: " + err.Error())
	}
}

func (c *Config) codexDaemonLog() string { return filepath.Join(c.Dir, "codex-daemon.log") }

// logCodexDaemon records daemon lifecycle steps for diagnosing switches. It
// never records command output, which can contain sensitive data.
func (c *Config) logCodexDaemon(line string) {
	path := c.codexDaemonLog()
	if info, err := os.Stat(path); err == nil && info.Size() > 256<<10 {
		os.Remove(path)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", c.now().UTC().Format(time.RFC3339), line)
}

func (c *Config) codexExecutable() (string, error) {
	if c.CodexBinary != "" {
		return exec.LookPath(c.CodexBinary)
	}
	if binary, err := exec.LookPath("codex"); err == nil {
		return binary, nil
	}
	// Finder-launched apps do not inherit the user's interactive shell PATH.
	home, _ := os.UserHomeDir()
	for _, path := range []string{filepath.Join(home, ".local", "bin", "codex"), "/opt/homebrew/bin/codex", "/usr/local/bin/codex"} {
		if binary, err := exec.LookPath(path); err == nil {
			return binary, nil
		}
	}
	return "", exec.ErrNotFound
}

func (c *Config) runCodexDaemonCommand(ctx context.Context, binary, action string, timeout time.Duration) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "daemon", action)
	// Match the home whose auth.json we just saved, even if Config was built
	// separately from the process environment. Never pass tokens as arguments.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.EqualFold(key, "CODEX_HOME") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+c.CodexHome)
	// Diagnostics are used only to recognize stopped/unsupported daemons and
	// never surfaced to callers, since arbitrary command output can be sensitive.
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	cmd.WaitDelay = time.Second
	start := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		c.logCodexDaemon(fmt.Sprintf("daemon %s: %v after %s", action, ctx.Err(), time.Since(start).Round(time.Millisecond)))
		return nil, "", ctx.Err()
	}
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	c.logCodexDaemon(fmt.Sprintf("daemon %s: %s after %s", action, outcome, time.Since(start).Round(time.Millisecond)))
	return out.Bytes(), diagnostic.String(), err
}
