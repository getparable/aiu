package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
}
