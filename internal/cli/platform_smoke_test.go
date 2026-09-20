package cli

// These tests deliberately run the CLI in a child process. They exercise the
// same entry point as the released binary while keeping all provider traffic on
// a local synthetic API.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getparable/aiu/internal/core"
)

const smokeHelperEnv = "AIU_PLATFORM_SMOKE_HELPER"

func TestHelperProcess(t *testing.T) {
	if os.Getenv(smokeHelperEnv) != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	cfg := testSmokeConfig()
	os.Exit(run(args, cfg))
}

func testSmokeConfig() *core.Config {
	// Keep the helper independent of the host's credentials. The Windows build
	// uses DPAPI by default; Linux uses its file-store implementation.
	root := os.Getenv("AIU_PLATFORM_SMOKE_DIR")
	cfg := core.DefaultConfig()
	cfg.Dir = filepath.Join(root, "aiu")
	cfg.ClaudeDir = filepath.Join(root, "claude")
	cfg.ClaudeGlobalConfig = filepath.Join(root, "claude.json")
	cfg.CodexHome = filepath.Join(root, "codex")
	cfg.UseKeychain = false
	cfg.UseDPAPI = runtime.GOOS == "windows"
	cfg.ClaudeService = "aiu-platform-smoke-never-real"
	api := os.Getenv("AIU_PLATFORM_SMOKE_API")
	cfg.ClaudeUsageURL = api + "/claude/usage"
	cfg.ClaudeProfileURL = api + "/claude/profile"
	cfg.ClaudeTokenURLs = []string{api + "/claude/token"}
	cfg.ClaudeAuthorizeURL = api + "/claude/authorize"
	cfg.ClaudeAuthorizeConsoleURL = api + "/claude/authorize-console"
	cfg.ClaudeManualRedirectURL = api + "/claude/manual"
	cfg.CodexUsageURL = api + "/codex/usage"
	cfg.CodexTokenURL = api + "/codex/token"
	cfg.CodexAuthorizeURL = api + "/codex/authorize"
	cfg.ReleaseAPIURL = api + "/releases/latest"
	cfg.HTTP = &http.Client{Timeout: 5 * time.Second}
	return cfg
}

type smokeAPI struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	methods []string
	paths   []string
}

func (a *smokeAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, got := range a.paths {
		if got == path {
			n++
		}
	}
	return n
}

func newSmokeAPI(t *testing.T) *smokeAPI {
	a := &smokeAPI{t: t}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.methods = append(a.methods, r.Method)
		a.paths = append(a.paths, r.URL.Path)
		a.mu.Unlock()
		if (strings.HasPrefix(r.URL.Path, "/claude/") || strings.HasPrefix(r.URL.Path, "/codex/")) && !strings.HasSuffix(r.URL.Path, "/token") {
			if got := r.Header.Get("Authorization"); got == "" {
				t.Errorf("%s %s missing Authorization", r.Method, r.URL.Path)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/claude/token":
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "synthetic-second") {
				fmt.Fprint(w, `{"access_token":"claude-second-access-secret","refresh_token":"claude-second-refresh-secret","expires_in":3600,"scope":"user:profile user:inference"}`)
			} else {
				fmt.Fprint(w, `{"access_token":"claude-access-secret","refresh_token":"claude-refresh-secret","expires_in":3600,"scope":"user:profile user:inference"}`)
			}
		case "/claude/profile":
			if strings.Contains(r.Header.Get("Authorization"), "claude-import-access-secret") {
				fmt.Fprint(w, `{"account":{"email":"claude-three@example.test","uuid":"claude-user-three"},"organization":{"uuid":"claude-org-three","name":"Synthetic Org Three"}}`)
			} else if strings.Contains(r.Header.Get("Authorization"), "claude-second-access-secret") || strings.Contains(r.Header.Get("Authorization"), "claude-newer-access-secret") {
				fmt.Fprint(w, `{"account":{"email":"claude-two@example.test","uuid":"claude-user-two"},"organization":{"uuid":"claude-org-two","name":"Synthetic Org Two"}}`)
			} else {
				fmt.Fprint(w, `{"account":{"email":"claude@example.test","uuid":"claude-user"},"organization":{"uuid":"claude-org","name":"Synthetic Org"}}`)
			}
		case "/claude/usage":
			fmt.Fprint(w, `{"five_hour":{"utilization":12,"resets_at":"2030-01-01T00:00:00Z"},"seven_day":{"utilization":23,"resets_at":"2030-01-02T00:00:00Z"}}`)
		case "/codex/token":
			_, _ = io.Copy(io.Discard, r.Body)
			fmt.Fprintf(w, `{"access_token":"codex-access-secret","refresh_token":"codex-refresh-secret","id_token":"%s","expires_in":3600}`, syntheticJWT())
		case "/codex/usage":
			fmt.Fprint(w, `{"rate_limit":{"primary_window":{"used_percent":17,"reset_at":1893456000,"limit_window_seconds":18000},"secondary_window":{"used_percent":29,"reset_at":1893542400,"limit_window_seconds":604800}}}`)
		case "/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v0.1.3","html_url":"https://example.test/aiu/releases/v0.1.3"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func syntheticJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"codex@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"codex-org","chatgpt_user_id":"codex-user","chatgpt_plan_type":"plus"}}`))
	return header + "." + payload + ".synthetic"
}

func runSmoke(t *testing.T, root, api string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
	cmd.Env = append(os.Environ(), smokeHelperEnv+"=1", "AIU_PLATFORM_SMOKE_DIR="+root, "AIU_PLATFORM_SMOKE_API="+api)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("CLI output leaked a synthetic token: %s", out.String())
	}
	return out.String(), err
}

func runManualLogin(t *testing.T, root, api string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
	cmd.Env = append(os.Environ(), smokeHelperEnv+"=1", "AIU_PLATFORM_SMOKE_DIR="+root, "AIU_PLATFORM_SMOKE_API="+api)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	var all strings.Builder
	var authURL string
	for scanner.Scan() {
		line := scanner.Text()
		all.WriteString(line + "\n")
		if strings.Contains(line, "http") && strings.Contains(line, "state=") {
			authURL = strings.TrimSpace(line)
			break
		}
	}
	if authURL == "" {
		t.Fatalf("login did not print authorization URL: %s", all.String())
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("authorization URL has no state")
	}
	code := "synthetic-code"
	for _, arg := range args {
		if arg == "claude-two" {
			code = "synthetic-second"
		}
	}
	_, _ = io.WriteString(stdin, code+"#"+state+"\n")
	_ = stdin.Close()
	remaining, _ := io.ReadAll(stdout)
	all.Write(remaining)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("login failed: %v\n%s", err, all.String())
	}
	return all.String()
}

func runBrowserLogin(t *testing.T, root, api string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
	cmd.Env = append(os.Environ(), smokeHelperEnv+"=1", "AIU_PLATFORM_SMOKE_DIR="+root, "AIU_PLATFORM_SMOKE_API="+api)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	var all strings.Builder
	var authURL string
	for scanner.Scan() {
		line := scanner.Text()
		all.WriteString(line + "\n")
		if strings.Contains(line, "http") && strings.Contains(line, "state=") {
			authURL = strings.TrimSpace(line)
			break
		}
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("browser login URL: %v (%q)", err, authURL)
	}
	redirect, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil || redirect.Host == "" {
		t.Fatalf("browser login redirect: %v", err)
	}
	query := redirect.Query()
	query.Set("code", "synthetic-code")
	query.Set("state", u.Query().Get("state"))
	redirect.RawQuery = query.Encode()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(redirect.String())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = response.Body.Close()
	remaining, _ := io.ReadAll(stdout)
	all.Write(remaining)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("browser login failed: %v\n%s", err, all.String())
	}
	return all.String()
}

func TestPlatformCLISmoke(t *testing.T) {
	if os.Getenv(smokeHelperEnv) == "1" {
		return
	}
	api := newSmokeAPI(t)
	root := t.TempDir()
	claudeOut := runManualLogin(t, root, api.srv.URL, "login", "--manual", "--no-open", "--label", "claude-one")
	if !strings.Contains(claudeOut, "claude@example.test") {
		t.Fatalf("Claude login output: %s", claudeOut)
	}
	codexOut := runBrowserLogin(t, root, api.srv.URL, "login", "--codex", "--no-open", "--label", "codex-one")
	if !strings.Contains(codexOut, "codex@example.test") {
		t.Fatalf("Codex login output: %s", codexOut)
	}
	status, err := runSmoke(t, root, api.srv.URL, "status", "--json", "--no-sync")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	var decoded []map[string]any
	if json.Unmarshal([]byte(status), &decoded) != nil || len(decoded) != 2 || !strings.Contains(status, "claude@example.test") || !strings.Contains(status, "codex@example.test") {
		t.Fatalf("bad status JSON: %s", status)
	}
	second := runManualLogin(t, root, api.srv.URL, "login", "--manual", "--no-open", "--label", "claude-two")
	if !strings.Contains(second, "claude-two@example.test") {
		t.Fatalf("second Claude login output: %s", second)
	}
	claudeFile := filepath.Join(root, "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeFile, []byte(`{"mcpOAuth":{"keep":"sibling"},"claudeAiOauth":{"accessToken":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSmoke(t, root, api.srv.URL, "switch", "claude-two"); err != nil || !strings.Contains(out, "claude-two@example.test") {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	updated, err := os.ReadFile(claudeFile)
	if err != nil || !strings.Contains(string(updated), "sibling") {
		t.Fatalf("switch dropped sibling fields: %v\n%s", err, updated)
	}
	if out, err := runSmoke(t, root, api.srv.URL, "sync"); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	// Simulate Claude Code rotating its token outside AIU. sync must adopt the
	// newer live credential, including when the store is DPAPI-backed on Windows.
	if err := os.WriteFile(claudeFile, []byte(`{"mcpOAuth":{"keep":"sibling"},"claudeAiOauth":{"accessToken":"claude-newer-access-secret","refreshToken":"claude-newer-refresh-secret","expiresAt":4102444800000,"scopes":["user:profile","user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSmoke(t, root, api.srv.URL, "sync"); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("sync newer token: %v\n%s", err, out)
	}
	cfg := testSmokeConfigForTest(root, api.srv.URL)
	idx, err := cfg.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	records, err := cfg.LoadRecords(idx)
	if err != nil {
		t.Fatal(err)
	}
	foundNewer := false
	for _, record := range records {
		if record.Provider == core.Claude && record.Email == "claude-two@example.test" && record.AccessToken == "claude-newer-access-secret" {
			foundNewer = true
		}
	}
	if !foundNewer {
		t.Fatalf("sync did not adopt newer Claude Code token: %+v", records)
	}
	// Import the current Claude Code login through the public add command too.
	if err := os.WriteFile(claudeFile, []byte(`{"mcpOAuth":{"keep":"sibling"},"claudeAiOauth":{"accessToken":"claude-import-access-secret","refreshToken":"claude-import-refresh-secret","expiresAt":4102444800000,"scopes":["user:profile","user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSmoke(t, root, api.srv.URL, "add", "--label", "imported"); err != nil || !strings.Contains(out, "added") || !strings.Contains(out, "imported") {
		t.Fatalf("add/import: %v\n%s", err, out)
	}
	before := api.count("/claude/usage") + api.count("/codex/usage")
	if _, err := runSmoke(t, root, api.srv.URL, "status", "--json", "--no-sync"); err != nil {
		t.Fatal(err)
	}
	afterFirst := api.count("/claude/usage") + api.count("/codex/usage")
	if _, err := runSmoke(t, root, api.srv.URL, "status", "--json", "--no-sync"); err != nil {
		t.Fatal(err)
	}
	afterSecond := api.count("/claude/usage") + api.count("/codex/usage")
	if afterFirst < before || afterSecond != afterFirst {
		t.Fatalf("shared five-minute cache did not suppress second status fetch: %d -> %d -> %d", before, afterFirst, afterSecond)
	}
	update, err := runSmoke(t, root, api.srv.URL, "update", "--json", "--force")
	if err != nil || !strings.Contains(update, "current") {
		t.Fatalf("update: %v\n%s", err, update)
	}
	if _, err := runSmoke(t, root, api.srv.URL, "remove", "claude-one"); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestWatchStopsWhenContextIsCanceled(t *testing.T) {
	root := t.TempDir()
	api := newSmokeAPI(t)
	_ = os.Setenv("AIU_PLATFORM_SMOKE_DIR", root)
	_ = os.Setenv("AIU_PLATFORM_SMOKE_API", api.srv.URL)
	cfg := testSmokeConfig()
	_ = os.Unsetenv("AIU_PLATFORM_SMOKE_DIR")
	_ = os.Unsetenv("AIU_PLATFORM_SMOKE_API")
	a := &app{cfg: cfg, opts: &options{interval: 15, noSync: true}, p: painter{}, stdout: io.Discard, stderr: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- a.watch(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop after context cancellation")
	}
}

func testSmokeConfigForTest(root, api string) *core.Config {
	previousRoot, previousAPI := os.Getenv("AIU_PLATFORM_SMOKE_DIR"), os.Getenv("AIU_PLATFORM_SMOKE_API")
	_ = os.Setenv("AIU_PLATFORM_SMOKE_DIR", root)
	_ = os.Setenv("AIU_PLATFORM_SMOKE_API", api)
	cfg := testSmokeConfig()
	_ = os.Setenv("AIU_PLATFORM_SMOKE_DIR", previousRoot)
	_ = os.Setenv("AIU_PLATFORM_SMOKE_API", previousAPI)
	return cfg
}
