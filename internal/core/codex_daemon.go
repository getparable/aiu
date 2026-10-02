package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

func codexDaemonUnchanged() *CodexDaemonResult {
	return &CodexDaemonResult{Status: "unchanged", Message: "Saved login unchanged; Codex daemon was not restarted. If a connected session still uses another account, run `codex app-server daemon restart`. " + codexResumeGuidance}
}

func (c *Config) restartCodexDaemon(ctx context.Context) *CodexDaemonResult {
	binary, err := c.codexExecutable()
	if err != nil {
		return &CodexDaemonResult{Status: "unavailable", Message: "Codex executable unavailable; only the saved login changed. Set AIU_CODEX_BIN to its path to enable daemon restarts. " + codexResumeGuidance, Warning: true}
	}
	output, err := c.runCodexDaemonCommand(ctx, binary, "version", 5*time.Second)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && ctx.Err() == nil && exitErr.Exited() {
			// Codex reports a stopped daemon with a nonzero exit code. Older
			// versions without daemon management also reject this command.
			return &CodexDaemonResult{Status: "not_running", Message: "No running compatible Codex daemon detected; only the saved login changed. " + codexResumeGuidance}
		}
		return &CodexDaemonResult{Status: "unavailable", Message: "Login saved, but Codex daemon status could not be checked. Run `codex app-server daemon restart` for connected sessions. " + codexResumeGuidance, Warning: true}
	}
	var version struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(output, &version) != nil || version.Status == "" {
		return &CodexDaemonResult{Status: "unavailable", Message: "Login saved, but Codex returned an unrecognized daemon status. Run `codex app-server daemon restart` for connected sessions. " + codexResumeGuidance, Warning: true}
	}
	if version.Status != "running" {
		return &CodexDaemonResult{Status: "not_running", Message: "Codex daemon is not running; only the saved login changed. " + codexResumeGuidance}
	}
	if _, err := c.runCodexDaemonCommand(ctx, binary, "restart", 30*time.Second); err != nil {
		return &CodexDaemonResult{Status: "restart_failed", Message: "Login saved, but the Codex daemon restart failed. Connected sessions may still use the previous account. Run `codex app-server daemon restart` to retry. " + codexResumeGuidance, Warning: true}
	}
	return &CodexDaemonResult{Status: "restarted", Message: "Codex daemon restarted. Connected terminals will reconnect; current turns were interrupted. " + codexResumeGuidance}
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

func (c *Config) runCodexDaemonCommand(ctx context.Context, binary, action string, timeout time.Duration) ([]byte, error) {
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
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out.Bytes(), err
}
