package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getparable/aiu/internal/core"
)

func TestAgyCLIHookAndStatus(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cfg := &core.Config{
		Dir:             filepath.Join(dir, "aiu"),
		AgySettingsFile: filepath.Join(dir, "antigravity-cli", "settings.json"),
		Now:             func() time.Time { return now },
		Warn:            func(string) {},
		Info:            func(string) {},
	}

	// 1. `aiu add --agy` configures settings.json.
	t.Setenv("AIU_HOOK_BIN", "~/.local/bin/aiu")
	opts, err := parse([]string{"add", "--agy"})
	if err != nil {
		t.Fatalf("parse add --agy: %v", err)
	}
	var stdout, stderr bytes.Buffer
	a := &app{cfg: cfg, opts: opts, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := a.add(context.Background()); err != nil {
		t.Fatalf("add --agy failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "statusLine hook") {
		t.Fatalf("unexpected add --agy output: %q", stdout.String())
	}

	// 2. `aiu hook agy` ingests stdin JSON silently (zero stdout).
	stdout.Reset()
	opts, err = parse([]string{"hook", "agy"})
	if err != nil {
		t.Fatalf("parse hook agy: %v", err)
	}
	stdinJSON := `{"product":"antigravity","email":"user@gmail.com","plan_tier":"Pro","quota":{"gemini-weekly":{"remaining_fraction":0.8,"reset_time":"2026-11-29T00:00:00Z"}}}`
	a = &app{cfg: cfg, opts: opts, stdin: strings.NewReader(stdinJSON), stdout: &stdout, stderr: &stderr}
	if err := a.hook(context.Background()); err != nil {
		t.Fatalf("hook agy failed: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("hook agy must not print to stdout, got %q", stdout.String())
	}

	// 3. `aiu --agy --json` returns the tracked account as read-only.
	stdout.Reset()
	opts, err = parse([]string{"--agy", "--json"})
	if err != nil {
		t.Fatalf("parse --agy --json: %v", err)
	}
	a = &app{cfg: cfg, opts: opts, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := a.status(context.Background()); err != nil {
		t.Fatalf("status --agy --json failed: %v", err)
	}
	var rows []jsonAccount
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode status json: %v\n%s", err, stdout.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Provider != core.Antigravity || row.Email != "user@gmail.com" || !row.ReadOnly || row.CanSwitch || row.Tier != "Google AI Pro" {
		t.Fatalf("unexpected json row: %+v", row)
	}
	if len(row.Windows) != 1 || row.Windows[0].Label != "7d Gemini" || row.Windows[0].Percent != 20 {
		t.Fatalf("unexpected windows: %+v", row.Windows)
	}

	// 4. `aiu frontend add --provider agy --contract-version 1` succeeds cleanly.
	stdout.Reset()
	opts, err = parse([]string{"frontend", "add", "--provider", "agy", "--contract-version", "1"})
	if err != nil {
		t.Fatalf("parse frontend add: %v", err)
	}
	code := runFrontend(opts, cfg, strings.NewReader(""), &stdout)
	if code != 0 {
		t.Fatalf("frontend add --provider agy exited %d: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"ok":true`) {
		t.Fatalf("expected ok result from frontend add, got: %s", stdout.String())
	}

	// 5. Custom statusLine conflict returns an error so Mac app surfaces the conflict.
	customCfg := &core.Config{
		Dir:             filepath.Join(dir, "aiu-custom"),
		AgySettingsFile: filepath.Join(dir, "custom-settings.json"),
		Now:             func() time.Time { return now },
		Warn:            func(string) {},
		Info:            func(string) {},
	}
	if err := writeTestFile(customCfg.AgySettingsFile, `{"statusLine":{"type":"command","command":"~/my-statusline.sh"}}`); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	opts, _ = parse([]string{"add", "--agy"})
	a = &app{cfg: customCfg, opts: opts, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	if err := a.add(context.Background()); err == nil || !strings.Contains(err.Error(), "custom statusLine") {
		t.Fatalf("expected custom statusLine conflict error, got %v", err)
	}
}

func TestResolveAiuHookBinaryPrefersUpgradeStablePath(t *testing.T) {
	cellarExe := "/opt/homebrew/Cellar/aiu/0.4.0/AIU.app/Contents/MacOS/aiu"
	got := resolveAiuHookBinaryFrom("/Users/dev", cellarExe, func(p string) bool {
		return filepath.ToSlash(p) == "/opt/homebrew/bin/aiu"
	})
	if filepath.ToSlash(got) != "/opt/homebrew/bin/aiu" {
		t.Fatalf("resolveAiuHookBinaryFrom(Cellar) = %q, want /opt/homebrew/bin/aiu", got)
	}

	gotLocal := resolveAiuHookBinaryFrom("/Users/dev", cellarExe, func(p string) bool {
		s := filepath.ToSlash(p)
		return s == "/Users/dev/.local/bin/aiu" || s == "/opt/homebrew/bin/aiu"
	})
	if gotLocal != "~/.local/bin/aiu" {
		t.Fatalf("resolveAiuHookBinaryFrom(.local/bin) = %q, want ~/.local/bin/aiu", gotLocal)
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
