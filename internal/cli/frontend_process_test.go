package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type frontendChild struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	events    <-chan frontendEvent
	readError <-chan error
}

func startFrontendChild(t *testing.T, root, api string, args ...string) *frontendChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--", "frontend"}, args...)...)
	cmd.Env = append(os.Environ(), smokeHelperEnv+"=1", "AIU_PLATFORM_SMOKE_DIR="+root, "AIU_PLATFORM_SMOKE_API="+api)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	events := make(chan frontendEvent, 32)
	readError := make(chan error, 1)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			var e frontendEvent
			if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
				readError <- err
				return
			}
			// The synthetic credentials deliberately do not look like provider
			// tokens: this asserts data separation, not only a redaction regex.
			if strings.Contains(scanner.Text(), "access-secret") || strings.Contains(scanner.Text(), "refresh-secret") {
				readError <- io.ErrUnexpectedEOF
				return
			}
			events <- e
		}
		readError <- scanner.Err()
	}()
	t.Cleanup(func() {
		in.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &frontendChild{cmd, in, events, readError}
}

func (c *frontendChild) until(t *testing.T, predicate func(frontendEvent) bool) frontendEvent {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-c.events:
			if !ok {
				t.Fatalf("child closed output: %v", <-c.readError)
			}
			if e.Version != 1 {
				t.Fatalf("invalid version: %d", e.Version)
			}
			if predicate(e) {
				return e
			}
		case <-timer.C:
			t.Fatal("frontend child did not finish its expected phase")
		}
	}
}

func (c *frontendChild) finish(t *testing.T, wantExit int) frontendEvent {
	t.Helper()
	e := c.until(t, func(e frontendEvent) bool { return e.Event == "result" })
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		if c.cmd.ProcessState.ExitCode() != wantExit {
			t.Fatalf("exit=%d want=%d result=%+v", c.cmd.ProcessState.ExitCode(), wantExit, e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frontend child emitted a result but did not exit")
	}
	return e
}

func TestFrontendChildCancellationAndParentEOF(t *testing.T) {
	for _, control := range []string{"cancel", "eof", "malformed", "version", "oversized"} {
		t.Run(control, func(t *testing.T) {
			probe, err := net.Listen("tcp", "127.0.0.1:54545")
			if err != nil {
				t.Skip("another login owns the fixed Claude callback port")
			}
			probe.Close()
			c := startFrontendChild(t, t.TempDir(), "http://127.0.0.1:1", "login", "--no-open")
			c.until(t, func(e frontendEvent) bool { return e.Event == "progress" && e.Phase == "waiting" })
			wantExit := 130
			switch control {
			case "cancel":
				_, err = io.WriteString(c.in, "{\"cancel\":true}\n")
			case "eof":
				err = c.in.Close()
			case "malformed":
				_, err = io.WriteString(c.in, "not-json\n")
				wantExit = 2
			case "version":
				_, err = io.WriteString(c.in, "{\"version\":99,\"cancel\":true}\n")
				wantExit = 2
			case "oversized":
				_, err = io.WriteString(c.in, strings.Repeat("x", 9000)+"\n")
				wantExit = 2
			}
			if err != nil {
				t.Fatal(err)
			}
			e := c.finish(t, wantExit)
			if e.OK || e.Cancelled != (wantExit == 130) {
				t.Fatalf("incorrect terminal outcome: %+v", e)
			}
			probe, err = net.Listen("tcp", "127.0.0.1:54545")
			if err != nil {
				t.Fatalf("callback listener survived child cancellation: %v", err)
			}
			probe.Close()
		})
	}
}

func TestFrontendChildrenShareCLIStateAndCache(t *testing.T) {
	root := t.TempDir()
	api := newSmokeAPI(t)
	c := startFrontendChild(t, root, api.srv.URL, "status")
	e := c.finish(t, 0)
	if !e.OK || e.Accounts == nil || len(e.Accounts) != 0 {
		t.Fatalf("empty snapshot: %+v", e)
	}
	path := filepath.Join(root, "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"claude-import-access-secret","refreshToken":"claude-import-refresh-secret","expiresAt":4102444800000,"scopes":["user:profile","user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c = startFrontendChild(t, root, api.srv.URL, "add", "--label", "fixture")
	e = c.finish(t, 0)
	if !e.OK || len(e.Accounts) != 1 || e.Accounts[0].Email != "claude-three@example.test" {
		t.Fatalf("add snapshot: %+v", e)
	}
	for i := 0; i < 2; i++ {
		c = startFrontendChild(t, root, api.srv.URL, "status")
		if e = c.finish(t, 0); !e.OK || len(e.Accounts) != 1 {
			t.Fatalf("status: %+v", e)
		}
	}
	if got := api.count("/claude/usage"); got != 1 {
		t.Fatalf("frontend multiplied usage requests: %d", got)
	}
	if _, err := runSmoke(t, root, api.srv.URL, "status", "--json"); err != nil {
		t.Fatal(err)
	}
	if got := api.count("/claude/usage"); got != 1 {
		t.Fatalf("CLI did not share frontend cache: %d", got)
	}
	c = startFrontendChild(t, root, api.srv.URL, "remove", "claude:claude-three@example.test#claude-org-three")
	if e = c.finish(t, 0); !e.OK || e.Accounts == nil || len(e.Accounts) != 0 {
		t.Fatalf("remove: %+v", e)
	}
	c = startFrontendChild(t, root, api.srv.URL, "switch", "missing")
	if e = c.finish(t, 1); e.OK || e.Error == nil || e.Error.Code != "operation_failed" {
		t.Fatalf("failure: %+v", e)
	}
	c = startFrontendChild(t, root, api.srv.URL, "status", "--invalid-private-value")
	if e = c.finish(t, 2); e.Error == nil || e.Error.Code != "invalid_input" {
		t.Fatalf("invalid flags: %+v", e)
	}
}
