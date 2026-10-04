//go:build !windows

package core

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCodexDaemonRestartIgnoresTerminalInterrupts(t *testing.T) {
	// Ctrl-C reaches the whole foreground process group. A restart CLI killed
	// mid-drain strands the daemon, so it must run in its own group.
	c := fakeDaemonConfig(t, `ps -o pgid= -p $$ > "$CODEX_HOME/pgid"; `+replaced("202"))
	if res := c.restartCodexDaemon(context.Background()); res.Status != "restarted" {
		t.Fatalf("result = %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(c.CodexHome, "pgid"))
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("restart CLI shares aiu's process group, so Ctrl-C would kill it")
	}
}
