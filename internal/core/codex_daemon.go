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
// (just over 12 minutes at most), and storage.
const codexSwitchLockWait = 14 * time.Minute

// A graceful restart drains active turns before replacing the daemon. When it
// outlives codexRestartWait, a second restart forces a draining daemon out.
// If the first attempt never reached the daemon (for example, it waited on
// Codex's lifecycle lock), the second performs the whole restart, so its budget
// covers that lock (375s), Codex's longest shutdown grace (300s), the forced
// kill (10s), and startup (20s), with margin. A forced restart normally
// finishes in about a second.
const (
	codexRestartWait = 10 * time.Second
	codexForceWait   = 12 * time.Minute
)

func codexDaemonUnchanged() *CodexDaemonResult {
	return &CodexDaemonResult{Status: "unchanged", Message: "Saved login unchanged; Codex daemon was not restarted. If a connected session still uses another account, run `codex app-server daemon restart`. " + codexResumeGuidance}
}

// restartCodexDaemon restarts a running daemon and records whether a later
// switch must retry it, since a half-finished restart leaves Codex refusing
// new sessions ("Server is draining") until another restart completes.
func (c *Config) restartCodexDaemon(ctx context.Context) *CodexDaemonResult {
	binary, err := c.codexExecutable()
	if err != nil {
		// Without an executable AIU can never reconcile, so retrying is moot.
		c.clearCodexRestartPending()
		return &CodexDaemonResult{Status: "unavailable", Message: "Codex executable unavailable; only the saved login changed. Set AIU_CODEX_BIN to its path to enable daemon restarts. " + codexResumeGuidance, Warning: true}
	}
	// Marked before starting so an interrupted probe or restart is retried.
	before := c.codexDaemonIdentity()
	c.markCodexRestartPending(before)
	res := c.reconcileCodexDaemon(ctx, binary, before.PID)
	switch res.Status {
	case "restarted", "not_running", "unsupported":
		c.clearCodexRestartPending()
	}
	c.logCodexDaemon("result " + res.Status)
	return res
}

// retryCodexRestart finishes a switch whose daemon restart did not complete.
// A daemon that replaced the recorded one started after the login was saved,
// for example after a manual restart, so it is not restarted again.
func (c *Config) retryCodexRestart(ctx context.Context) *CodexDaemonResult {
	var marked codexDaemonIdentity
	if data, err := os.ReadFile(c.codexRestartPendingFile()); err == nil && json.Unmarshal(data, &marked) == nil && marked.PID > 0 {
		if current := c.codexDaemonIdentity(); current.PID > 0 && current != marked {
			c.clearCodexRestartPending()
			c.logCodexDaemon(fmt.Sprintf("retry: daemon already replaced (pid %d -> %d)", marked.PID, current.PID))
			return &CodexDaemonResult{Status: "restarted", Message: "The Codex daemon was replaced after the login was saved, so connected terminals already use it. " + codexResumeGuidance}
		}
	}
	return c.restartCodexDaemon(ctx)
}

func (c *Config) reconcileCodexDaemon(ctx context.Context, binary string, before int) *CodexDaemonResult {
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
	c.Info("Restarting the Codex daemon so connected terminals use the saved login…")
	for attempt, wait := range []time.Duration{durationOr(c.CodexRestartWait, codexRestartWait), durationOr(c.CodexForceWait, codexForceWait)} {
		if attempt == 1 {
			c.Info("The Codex daemon is still finishing turns; restarting it again to stop them…")
		}
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
		return &CodexDaemonResult{Status: "restarted", Message: "Codex daemon restarted on a second attempt; turns that were still running were stopped. Connected terminals will reconnect; re-send any interrupted turn. " + codexResumeGuidance}
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

// codexDaemonIdentity names a daemon process. The start time tells a new
// daemon apart from an old one whose PID was reused.
type codexDaemonIdentity struct {
	PID   int    `json:"pid"`
	Start string `json:"processStartTime,omitempty"`
}

// codexDaemonIdentity reads the managed daemon's PID record (daemon.pid, or
// app-server.pid for standalone installs); it is zero when absent.
func (c *Config) codexDaemonIdentity() codexDaemonIdentity {
	for _, name := range []string{"daemon.pid", "app-server.pid"} {
		data, err := os.ReadFile(filepath.Join(c.CodexHome, "app-server-daemon", name))
		if err != nil {
			continue
		}
		var record codexDaemonIdentity
		if json.Unmarshal(data, &record) == nil && record.PID > 0 {
			return record
		}
	}
	return codexDaemonIdentity{}
}

func (c *Config) codexRestartPendingFile() string {
	return filepath.Join(c.CodexHome, ".aiu-daemon-restart-pending")
}

func (c *Config) codexRestartPending() bool {
	_, err := os.Stat(c.codexRestartPendingFile())
	return err == nil
}

// markCodexRestartPending records the daemon a restart must replace.
func (c *Config) markCodexRestartPending(daemon codexDaemonIdentity) {
	data, _ := json.Marshal(daemon)
	if err := os.WriteFile(c.codexRestartPendingFile(), data, 0o600); err != nil {
		c.Warn("could not record the unfinished Codex daemon restart: " + err.Error())
	}
}

func (c *Config) clearCodexRestartPending() { os.Remove(c.codexRestartPendingFile()) }

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
	detachFromTerminalSignals(cmd)
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
