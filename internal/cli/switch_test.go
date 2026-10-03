package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/getparable/aiu/internal/core"
)

func codexSwitchConfig(t *testing.T) *core.Config {
	t.Helper()
	cfg := core.DefaultConfig()
	cfg.Dir = t.TempDir()
	cfg.CodexHome = filepath.Join(cfg.Dir, "codex")
	cfg.CodexBinary = filepath.Join(cfg.Dir, "missing-codex")
	cfg.UseKeychain, cfg.UseDPAPI = false, false
	cfg.Warn = func(string) {}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_plan_type":"pro"}}`))
	record := &core.Record{
		Provider: core.Codex, Email: "fixture@example.test", Label: "work",
		OrgUUID: "fixture-account", AccountID: "fixture-account",
		AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh",
		IDToken:   "e30." + claims + ".synthetic",
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
	}
	for name, value := range map[string]any{
		"accounts.json": &core.Index{Version: 1, Accounts: []*core.IndexEntry{{Provider: core.Codex, Email: record.Email, Org: record.OrgUUID, Label: record.Label}}},
		"tokens.json":   map[string]*core.Record{record.StoreKey(): record},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestCodexSwitchReportsSavedLoginAndResume(t *testing.T) {
	cfg := codexSwitchConfig(t)
	for _, name := range []string{"changed", "already saved"} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			a := &app{cfg: cfg, opts: &options{args: []string{"codex:work"}}, stdout: &stdout, stderr: &stderr}
			if err := a.switchTo(context.Background()); err != nil {
				t.Fatal(err)
			}
			output := stdout.String()
			for _, want := range []string{"saved login", "codex resume --last --no-daemon"} {
				if !strings.Contains(output+stderr.String(), want) {
					t.Errorf("missing %q in switch result:\n%s\n%s", want, output, stderr.String())
				}
			}
			if live := cfg.ReadCodexAuth(); live == nil || live.LiveEmail() != "fixture@example.test" {
				t.Fatal("switch did not persist the selected login")
			}
		})
	}
}

func TestCodexSwitchDaemonOutcomes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex executable uses a POSIX shell")
	}
	for _, tt := range []struct {
		name, probe, restart, status, want string
		warning                            bool
		timeout                            bool
	}{
		{"running", `echo '{"status":"running"}'`, "exit 0", "restarted", "Connected terminals will reconnect", false, false},
		{"stopped", `echo '{"status":"stopped"}'`, "exit 99", "not_running", "only the saved login changed", false, false},
		{"probe failure", "echo 'permission denied: synthetic-secret' >&2; exit 2", "exit 99", "unavailable", "status could not be checked", true, false},
		{"older CLI", `echo "error: unrecognized subcommand 'daemon'" >&2; exit 2`, "exit 99", "unsupported", "no daemon management", false, false},
		{"absent daemon socket", `echo "Error: failed to connect to $CODEX_HOME/app-server-control/app-server-control.sock" >&2; echo 'No such file or directory (os error 2)' >&2; exit 1`, "exit 99", "not_running", "not running", false, false},
		{"restart failed", `echo '{"status":"running"}'`, "exit 9", "restart_failed", "restart failed", true, false},
		{"invalid status", `echo 'not JSON'`, "exit 99", "unavailable", "unrecognized daemon status", true, false},
		{"unknown status", `echo '{"status":"error"}'`, "exit 99", "unavailable", "unrecognized daemon status", true, false},
		{"probe cancelled", "exec sleep 30", "exit 99", "unavailable", "status could not be checked", true, true},
		{"restart cancelled", `echo '{"status":"running"}'`, "exec sleep 30", "restart_failed", "restart failed", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				cfg := codexSwitchConfig(t)
				cfg.CodexBinary = filepath.Join(cfg.Dir, "fake-codex")
				script := "#!/bin/sh\ncase \"$*\" in\n'app-server daemon version') " + tt.probe + " ;;\n'app-server daemon restart') echo restarted >> \"$CODEX_HOME/restarts\"; " + tt.restart + " ;;\n*) exit 99 ;;\nesac\n"
				if err := os.WriteFile(cfg.CodexBinary, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				a := &app{cfg: cfg, opts: &options{args: []string{"codex:work"}, json: asJSON}, stdout: &stdout, stderr: &stderr}
				ctx := context.Background()
				if tt.timeout {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
					defer cancel()
				}
				if err := a.switchTo(ctx); err != nil {
					t.Fatal(err)
				}
				if asJSON {
					var result core.SwitchResult
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.CodexDaemon == nil || result.CodexDaemon.Status != tt.status || result.CodexDaemon.Warning != tt.warning {
						t.Fatalf("daemon result = %+v", result.CodexDaemon)
					}
					if strings.Contains(stdout.String(), "synthetic-access") || strings.Contains(stdout.String(), "synthetic-refresh") {
						t.Fatal("JSON switch result exposed tokens")
					}
				} else if !strings.Contains(stdout.String()+stderr.String(), tt.want) {
					t.Fatalf("missing %q in result: %s %s", tt.want, stdout.String(), stderr.String())
				}
				if tt.warning != strings.Contains(stderr.String(), "warn:") {
					t.Fatalf("warning output = %q", stderr.String())
				}
				if strings.Contains(stdout.String()+stderr.String(), "synthetic-secret") {
					t.Fatal("probe diagnostic exposed command output")
				}
				if live := cfg.ReadCodexAuth(); live == nil || live.LiveEmail() != "fixture@example.test" {
					t.Fatal("daemon outcome lost the saved credentials")
				}
				_, restartErr := os.Stat(filepath.Join(cfg.CodexHome, "restarts"))
				if (tt.status == "restarted" || tt.status == "restart_failed") != (restartErr == nil) {
					t.Fatalf("unexpected restart attempt: %v", restartErr)
				}
			}
		})
	}
}

func TestCodexFailedCredentialWriteDoesNotTouchDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex executable uses a POSIX shell")
	}
	cfg := codexSwitchConfig(t)
	cfg.CodexBinary = filepath.Join(cfg.Dir, "fake-codex")
	if err := os.WriteFile(cfg.CodexBinary, []byte("#!/bin/sh\necho called >> \"$CODEX_HOME/calls\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// An existing directory prevents the atomic auth.json replacement.
	if err := os.MkdirAll(filepath.Join(cfg.CodexHome, "auth.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, opts: &options{args: []string{"codex:work"}}, stdout: &out, stderr: &out}
	if err := a.switchTo(context.Background()); err == nil {
		t.Fatal("credential write unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(cfg.CodexHome, "calls")); !os.IsNotExist(err) {
		t.Fatalf("failed save touched daemon: %v", err)
	}
}

func TestFrontendSwitchReportsDaemonWarning(t *testing.T) {
	cfg := codexSwitchConfig(t)
	message, err := frontendCommand(context.Background(), cfg, &options{args: []string{"switch", "codex:work"}}, "switch", func(frontendEvent) {})
	if err != nil || !strings.Contains(message, "account switched") || !strings.Contains(message, "executable unavailable") {
		t.Fatalf("frontend result = %q, %v", message, err)
	}
}

func TestSharedSwitchContract(t *testing.T) {
	cfg := codexSwitchConfig(t)
	for _, name := range []string{"switch-warning.json", "switch-unchanged.json"} {
		var stdout, stderr bytes.Buffer
		a := &app{cfg: cfg, opts: &options{args: []string{"codex:work"}, json: true}, stdout: &stdout, stderr: &stderr}
		if err := a.switchTo(context.Background()); err != nil {
			t.Fatal(err)
		}
		var result core.SwitchResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "tests", "fixtures", "frontend", name)
		if os.Getenv("AIU_UPDATE_CONTRACT_FIXTURES") == "1" {
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixture core.SwitchResult
		if err := json.Unmarshal(data, &fixture); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, fixture) {
			t.Fatalf("%s no longer matches the Swift switch contract", name)
		}
	}
}
