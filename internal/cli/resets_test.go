package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getparable/aiu/internal/core"
)

func TestResetCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"reset", "codex:work"}, {"reset", "--yes"}, {"reset", "one", "two", "--yes"},
		{"reset", "codex:work", "--yes=false"}, {"status", "--yes"},
		{"auto-reset", "codex:work"}, {"auto-reset", "codex:work", "--enabled", "yes"},
		{"resets", "codex:work", "--enabled", "true"}, {"resets", "codex:work", "--request-id", "unused"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			opts, err := parse(args)
			if err == nil {
				err = validateResetOptions(opts, opts.command)
			}
			if err == nil {
				t.Fatal("unsafe or incomplete command accepted")
			}
		})
	}
	for _, args := range [][]string{
		{"resets", "codex:work"}, {"reset", "codex:work", "--yes"},
		{"auto-reset", "codex:work", "--enabled=true"}, {"auto-reset", "codex:work", "--enabled", "false"},
	} {
		opts, err := parse(args)
		if err == nil {
			err = validateResetOptions(opts, opts.command)
		}
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestFrontendBankedResetContract(t *testing.T) {
	const selector = "codex:fixture@example.test#fixture-org"
	const requestID = "01234567-89ab-4cde-8fab-0123456789ab"
	var gets, posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-only-token" || r.Header.Get("ChatGPT-Account-Id") != "fixture-org" {
			t.Errorf("wrong authorization or account header")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/credits":
			gets++
			io.WriteString(w, `{"available_count":2,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2030-01-01T00:00:00Z","expires_at":null}]}`)
		case r.Method == "POST" && r.URL.Path == "/credits/consume":
			posts++
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if len(payload) != 2 || payload["redeem_request_id"] != requestID || payload["credit_id"] != "credit-1" {
				t.Errorf("wrong payload: %v", payload)
			}
			io.WriteString(w, `{"code":"reset","windows_reset":2}`)
		case r.Method == "GET" && r.URL.Path == "/usage":
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000},"secondary_window":null},"rate_limit_reset_credits":{"available_count":2}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := core.DefaultConfig()
	cfg.Dir = t.TempDir()
	cfg.CodexHome, cfg.ClaudeDir = filepath.Join(cfg.Dir, "codex"), filepath.Join(cfg.Dir, "claude")
	cfg.ClaudeGlobalConfig = filepath.Join(cfg.Dir, "claude-global.json")
	cfg.UseKeychain, cfg.UseDPAPI = false, false
	cfg.CodexResetCreditsURL, cfg.CodexUsageURL = server.URL+"/credits", server.URL+"/usage"
	cfg.HTTP = server.Client()
	cfg.Warn, cfg.Info = func(string) {}, func(string) {}
	record := &core.Record{Provider: core.Codex, Email: "fixture@example.test", OrgUUID: "fixture-org", AccountID: "fixture-org", AccessToken: "synthetic-only-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	for name, value := range map[string]any{
		"accounts.json": &core.Index{Version: 1, Accounts: []*core.IndexEntry{{Provider: core.Codex, Email: record.Email, Org: record.OrgUUID}}},
		"tokens.json":   map[string]*core.Record{record.StoreKey(): record},
	} {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoke := func(args ...string) frontendEvent {
		t.Helper()
		opts, err := parse(append([]string{"frontend"}, args...))
		if err != nil {
			t.Fatal(err)
		}
		in, parent := io.Pipe()
		defer in.Close()
		defer parent.Close()
		var out bytes.Buffer
		exit := runFrontend(opts, cfg, in, &out)
		if strings.Contains(out.String(), "synthetic-only-token") || strings.Contains(out.String(), "accessToken") {
			t.Fatal("credential leaked in frontend output")
		}
		events := decodeFrontend(t, out.Bytes())
		if len(events) < 2 || events[0].Event != "hello" || events[len(events)-1].Event != "result" {
			t.Fatalf("invalid sequence %s", out.String())
		}
		last := events[len(events)-1]
		if exit != 0 || !last.OK {
			t.Fatalf("exit=%d output=%s", exit, out.String())
		}
		return last
	}
	listed := invoke("resets", selector, "--provider", "codex", "--contract-version", "1")
	if len(listed.Accounts) != 1 || listed.Accounts[0].BankedResets == nil {
		t.Fatalf("missing reset presentation: %+v", listed)
	}
	bank := listed.Accounts[0].BankedResets
	if bank.AvailableCount == nil || *bank.AvailableCount != 2 || bank.AutoReset || !bank.CanRedeem {
		t.Fatalf("unexpected reset state: %+v", bank)
	}
	enabled := invoke("auto-reset", selector, "--enabled", "true", "--provider", "codex")
	if len(enabled.Accounts) != 1 || !enabled.Accounts[0].BankedResets.AutoReset {
		t.Fatalf("toggle not in snapshot: %+v", enabled)
	}
	if posts != 0 {
		t.Fatal("enabling at healthy usage spent a credit")
	}
	invoke("reset", selector, "--yes", "--credit-id", "credit-1", "--request-id", requestID, "--provider", "codex")
	invoke("reset", selector, "--yes", "--credit-id", "credit-1", "--request-id", requestID, "--provider", "codex")
	if gets != 1 || posts != 1 {
		t.Fatalf("request counts: GET=%d POST=%d", gets, posts)
	}
	if err := cfg.SetAutoReset(context.Background(), selector, core.Codex, false); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendResetRequiresConfirmationAndExplicitToggleValue(t *testing.T) {
	for _, args := range [][]string{
		{"frontend", "reset", "codex:fixture@example.test#fixture-org"},
		{"frontend", "auto-reset", "codex:fixture@example.test#fixture-org"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(err)
		}
		cfg := core.DefaultConfig()
		cfg.Dir = filepath.Join(t.TempDir(), "must-not-be-created")
		var out bytes.Buffer
		code := runFrontend(opts, cfg, bytes.NewReader(nil), &out)
		events := decodeFrontend(t, out.Bytes())
		if code != 2 || len(events) != 2 || events[1].Error == nil || events[1].Error.Code != "invalid_input" {
			t.Fatalf("code=%d events=%+v", code, events)
		}
		if _, err := os.Stat(cfg.Dir); !os.IsNotExist(err) {
			t.Fatalf("invalid reset mutated config: %v", err)
		}
	}
}
