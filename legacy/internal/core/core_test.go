package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI stands in for Anthropic and OpenAI: token, profile and usage endpoints.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	usage    map[string]string    // access token -> usage body
	status   map[string]int       // access token -> forced usage status
	emails   map[string]string    // access token -> Claude profile email
	orgs     map[string][2]string // access token -> {organization uuid, name}
	refresh  map[string]string    // refresh token -> next access token ("" = invalid_grant)
	usageHit atomic.Int32
	server   *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, usage: map[string]string{}, status: map[string]int{}, emails: map[string]string{}, orgs: map[string][2]string{}, refresh: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch r.URL.Path {
	case "/claude/usage", "/codex/usage":
		f.usageHit.Add(1)
		if s := f.status[token]; s != 0 {
			w.WriteHeader(s)
			return
		}
		body, ok := f.usage[token]
		if !ok {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, body)
	case "/claude/profile":
		email, ok := f.emails[token]
		if !ok {
			w.WriteHeader(401)
			return
		}
		org := f.orgs[token]
		if org[0] == "" {
			org = [2]string{"org-default", "Org"}
		}
		fmt.Fprintf(w, `{"account":{"email":%q,"uuid":"u-1"},"organization":{"uuid":%q,"name":%q}}`, email, org[0], org[1])
	case "/claude/token", "/codex/token":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		next, ok := f.refresh[body["refresh_token"]]
		if !ok || next == "" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		delete(f.refresh, body["refresh_token"]) // single use
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":28800}`, next, "rt-"+next)
	default:
		w.WriteHeader(404)
	}
}

func testConfig(t *testing.T, api *fakeAPI) *Config {
	dir := t.TempDir()
	now := time.UnixMilli(1_789_560_000_000)
	c := &Config{
		Dir:                filepath.Join(dir, "aiu"),
		StoreService:       "aiu-test",
		ClaudeDir:          filepath.Join(dir, "claude"),
		ClaudeService:      "aiu-test-nonexistent-service",
		ClaudeGlobalConfig: filepath.Join(dir, "claude.json"),
		CodexHome:          filepath.Join(dir, "codex"),
		ClaudeUsageURL:     api.server.URL + "/claude/usage",
		ClaudeProfileURL:   api.server.URL + "/claude/profile",
		ClaudeTokenURLs:    []string{api.server.URL + "/claude/token"},
		CodexUsageURL:      api.server.URL + "/codex/usage",
		CodexTokenURL:      api.server.URL + "/codex/token",
		HTTP:               api.server.Client(),
		Warn:               func(m string) { t.Log("warn:", m) },
		Info:               func(string) {},
	}
	var mu sync.Mutex
	c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() {})
	advance = func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	return c
}

var advance func(time.Duration)

func writeClaudeCreds(t *testing.T, c *Config, access, refresh string, expiresAt int64) {
	t.Helper()
	doc := map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": expiresAt, "subscriptionType": "max", "rateLimitTier": "default_claude_max_20x"},
		"mcpOAuth":      map[string]any{"keep": "me"},
	}
	data, _ := json.Marshal(doc)
	if err := os.MkdirAll(c.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.ClaudeDir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaudeCreds(t *testing.T, c *Config) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := readJSONFile(filepath.Join(c.ClaudeDir, ".credentials.json"), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func fakeJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

const claudeUsage = `{"five_hour":{"utilization":14.0,"resets_at":"2026-09-16T17:00:00+00:00"},"seven_day":{"utilization":99.0,"resets_at":"2026-09-21T08:00:00+00:00"},"seven_day_opus":null,"nimbus_quill":{"utilization":0.0,"resets_at":null},"extra_usage":{"is_enabled":false}}`
const codexUsage = `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":94,"limit_window_seconds":604800,"reset_at":1789842324},"secondary_window":null}}`

func TestNormalizeWindows(t *testing.T) {
	now := time.Unix(1_789_560_000, 0)
	cw := NormalizeWindows(json.RawMessage(claudeUsage), now)
	if len(cw) != 2 || cw[0].Label != "5h session" || cw[0].Group != "session" || cw[1].Label != "7d all" || cw[1].Percent != 99 {
		t.Fatalf("claude windows = %+v", cw)
	}
	if cw[1].Severity != "critical" {
		t.Errorf("99%% should be critical, got %q", cw[1].Severity)
	}
	xw := NormalizeWindows(json.RawMessage(codexUsage), now)
	if len(xw) != 1 || xw[0].Label != "7d all" || xw[0].Group != "weekly" || xw[0].Percent != 94 {
		t.Fatalf("codex windows = %+v", xw)
	}
	if h := HeadroomOf(xw); h.Session != 0 || h.Weekly != 94 {
		t.Errorf("headroom = %+v", h)
	}
}

func TestRedact(t *testing.T) {
	in := `failed: {"refresh_token":"abcdefghijklmnop"} Bearer sk-ant-oat01-xyz eyJhbGciOiJIUzI1NiJ9.payload`
	out := Redact(in)
	for _, leak := range []string{"abcdefghijklmnop", "sk-ant-oat01", "eyJhbGciOiJIUzI1NiJ9"} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted text still contains %q: %s", leak, out)
		}
	}
}

func TestClaudeKeychainService(t *testing.T) {
	// sha256 of the path as spelled, first 8 hex chars — checked against a real
	// CLAUDE_CONFIG_DIR login with Claude Code 2.1.273.
	if got := ClaudeKeychainService("/Users/example/.claude-accounts/work"); got != "Claude Code-credentials-ce139327" {
		t.Errorf("got %q", got)
	}
	if got := ClaudeKeychainService(""); got != "Claude Code-credentials" {
		t.Errorf("got %q", got)
	}
}

func TestCaptureRefreshHandBackAndThrottle(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()

	writeClaudeCreds(t, c, "at-1", "rt-1", c.now().Add(time.Hour).UnixMilli())
	api.emails["at-1"] = "a@example.com"
	api.usage["at-1"] = claudeUsage
	saved, err := c.CaptureClaudeCode(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.IsNew || saved.Record.Email != "a@example.com" || saved.Record.Label != "work" {
		t.Fatalf("saved = %+v", saved.Record)
	}

	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r := snap.Results[0]; r.Err != "" || !r.Active || len(r.Usage) == 0 {
		t.Fatalf("first collect = %+v", r)
	}
	if api.usageHit.Load() != 1 {
		t.Fatalf("usage hits = %d, want 1", api.usageHit.Load())
	}

	// Within the spacing: answered from the cache, nothing sent.
	advance(time.Minute)
	snap, _ = c.Collect(ctx, CollectOptions{})
	if api.usageHit.Load() != 1 || snap.Results[0].Err != "" || len(snap.Results[0].Usage) == 0 {
		t.Fatalf("cached collect sent a request or lost usage: hits=%d res=%+v", api.usageHit.Load(), snap.Results[0])
	}

	// Past the spacing, with the access token expired: refresh, and hand the rotated
	// pair back to Claude Code, which still holds the refresh token just spent.
	advance(2 * time.Hour)
	api.refresh["rt-1"] = "at-2"
	api.usage["at-2"] = claudeUsage
	api.emails["at-2"] = "a@example.com"
	snap, _ = c.Collect(ctx, CollectOptions{})
	if r := snap.Results[0]; r.Err != "" {
		t.Fatalf("refresh collect: %s", r.Err)
	}
	creds := readClaudeCreds(t, c)
	oauth := creds["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "at-2" || oauth["refreshToken"] != "rt-at-2" {
		t.Fatalf("Claude Code did not get the rotated pair: %+v", oauth)
	}
	if creds["mcpOAuth"] == nil {
		t.Fatal("write-back dropped a sibling key")
	}
	if oauth["subscriptionType"] != "max" {
		t.Fatal("write-back dropped subscriptionType")
	}
}

func TestRateLimitCooldown(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	writeClaudeCreds(t, c, "at-1", "rt-1", c.now().Add(24*time.Hour).UnixMilli())
	api.emails["at-1"] = "a@example.com"
	api.usage["at-1"] = claudeUsage
	if _, err := c.CaptureClaudeCode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if snap, _ := c.Collect(ctx, CollectOptions{}); snap.Results[0].Err != "" {
		t.Fatal(snap.Results[0].Err)
	}

	advance(6 * time.Minute)
	api.status["at-1"] = 429
	snap, _ := c.Collect(ctx, CollectOptions{})
	r := snap.Results[0]
	if r.Err != "" || !strings.Contains(r.Stale, "rate limited") || len(r.Usage) == 0 {
		t.Fatalf("429 should keep the last values with a note: %+v", r)
	}
	hits := api.usageHit.Load()

	// Still in the 10-minute cooldown after the spacing has passed: nothing is sent.
	advance(6 * time.Minute)
	c.Collect(ctx, CollectOptions{})
	if api.usageHit.Load() != hits {
		t.Fatal("a request went out during the cooldown")
	}

	// After the cooldown, with the doubled spacing that follows a recent 429.
	advance(11 * time.Minute)
	delete(api.status, "at-1")
	snap, _ = c.Collect(ctx, CollectOptions{})
	if api.usageHit.Load() != hits+1 || snap.Results[0].Stale != "" {
		t.Fatalf("expected one fresh request after the cooldown: hits=%d res=%+v", api.usageHit.Load(), snap.Results[0])
	}
}

func TestSwitchClaudeKeepsBothAccounts(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	exp := c.now().Add(24 * time.Hour).UnixMilli()

	if err := os.WriteFile(c.ClaudeGlobalConfig, []byte(`{"numStartups":3,"oauthAccount":{"emailAddress":"a@example.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClaudeCreds(t, c, "at-a", "rt-a", exp)
	api.emails["at-a"] = "a@example.com"
	if _, err := c.CaptureClaudeCode(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	// The user logs Claude Code in as b — the case that lost an account before.
	writeClaudeCreds(t, c, "at-b", "rt-b", exp)
	api.emails["at-b"] = "b@example.com"
	if _, err := c.CaptureClaudeCode(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	res, err := c.SwitchAccount(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyActive || res.UntrackedReplaced != "" {
		t.Fatalf("switch result = %+v", res)
	}
	oauth := readClaudeCreds(t, c)["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "at-a" {
		t.Fatalf("Claude Code should now hold a's token, has %v", oauth["accessToken"])
	}
	var global map[string]any
	readJSONFile(c.ClaudeGlobalConfig, &global)
	if global["numStartups"] != float64(3) || obj(global["oauthAccount"])["emailAddress"] != "a@example.com" {
		t.Fatalf(".claude.json = %v", global)
	}
	if st, _ := os.Stat(c.ClaudeGlobalConfig); st.Mode().Perm() != 0o644 {
		t.Errorf(".claude.json mode changed to %v", st.Mode().Perm())
	}

	api.usage["at-a"], api.usage["at-b"] = claudeUsage, strings.Replace(claudeUsage, "14.0", "3.0", 1)
	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil || len(snap.Results) != 2 || snap.Results[0].Err != "" || snap.Results[1].Err != "" {
		t.Fatalf("two-account collect: %v %+v", err, snap)
	}
	if !snap.Results[0].Active || snap.Results[1].Active {
		t.Fatalf("only a should be active: %v %v", snap.Results[0].Active, snap.Results[1].Active)
	}

	live, err := c.DescribeLive(ctx, false)
	if err != nil || live[Claude].Email != "a@example.com" || !live[Claude].Verified {
		t.Fatalf("live = %+v, %v", live[Claude], err)
	}
	res, err = c.SwitchAccount(ctx, "b", Claude)
	if err != nil || res.AlreadyActive {
		t.Fatalf("switch back: %+v %v", res, err)
	}
	if oauth := readClaudeCreds(t, c)["claudeAiOauth"].(map[string]any); oauth["accessToken"] != "at-b" {
		t.Fatal("switch back did not restore b")
	}
}

// One address can hold two Claude organizations with separate limits; they must not
// overwrite each other, and only the live one is active.
func TestTwoOrganizationsOnOneAddress(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	exp := c.now().Add(24 * time.Hour).UnixMilli()

	api.emails["at-personal"] = "info@example.com"
	api.orgs["at-personal"] = [2]string{"org-personal", "Personal"}
	api.emails["at-business"] = "info@example.com"
	api.orgs["at-business"] = [2]string{"org-business", "Business"}
	api.usage["at-personal"] = claudeUsage
	api.usage["at-business"] = strings.Replace(claudeUsage, "99.0", "12.0", 1)

	writeClaudeCreds(t, c, "at-personal", "rt-personal", exp)
	if _, err := c.CaptureClaudeCode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	writeClaudeCreds(t, c, "at-business", "rt-business", exp)
	if _, err := c.CaptureClaudeCode(ctx, ""); err != nil {
		t.Fatal(err)
	}

	idx, _ := c.LoadIndex()
	if len(idx.Accounts) != 2 {
		t.Fatalf("expected both organizations to be tracked, got %d", len(idx.Accounts))
	}
	// Names come from what the browser signed in as; neither add passed a label.
	if got := []string{idx.Accounts[0].Label, idx.Accounts[1].Label}; got[0] != "info" || got[1] != "business" {
		t.Fatalf("auto labels = %v, want [info business]", got)
	}

	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var active, idle *Result
	for _, r := range snap.Results {
		if r.Active {
			active = r
		} else {
			idle = r
		}
	}
	if active == nil || idle == nil {
		t.Fatalf("exactly one account should be active: %+v", snap.Results)
	}
	if active.Record.OrgName != "Business" {
		t.Errorf("the live login is the Business organization, got %q", active.Record.OrgName)
	}
	if HeadroomOf(NormalizeWindows(active.Usage, time.Now())).Weekly != 12 {
		t.Error("each organization must get its own usage reading")
	}
	if HeadroomOf(NormalizeWindows(idle.Usage, time.Now())).Weekly != 99 {
		t.Error("the other organization kept the wrong reading")
	}

	// A profile entry cached before organizations existed must not make both
	// organizations on the address look active.
	c.cacheUpdate(profileKey, func(e *cacheEntry) *cacheEntry { e.Org = ""; return e })
	snap, err = c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	activeCount := 0
	for _, r := range snap.Results {
		if r.Active {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("exactly one account may be active, got %d", activeCount)
	}

	// The panel passes the full key, which must resolve to exactly one organization.
	for _, r := range snap.Results {
		key := string(r.Record.Provider) + ":" + r.Record.Email + "#" + r.Record.OrgUUID
		_, entry, err := c.FindAccount(key, "")
		if err != nil || entry.Org != r.Record.OrgUUID {
			t.Fatalf("FindAccount(%q) = %+v, %v", key, entry, err)
		}
	}
	if _, _, err := c.FindAccount("claude:info@example.com#nope", ""); err == nil {
		t.Fatal("an unknown organization must not resolve")
	}

	// Switching to the personal organization writes its token into Claude Code.
	if _, err := c.SwitchAccount(ctx, idle.Record.Label, ""); err != nil {
		t.Fatal(err)
	}
	if oauth := readClaudeCreds(t, c)["claudeAiOauth"].(map[string]any); oauth["accessToken"] != "at-personal" {
		t.Fatalf("switch used the wrong organization: %v", oauth["accessToken"])
	}
}

// A personal organization is named "<address>'s Organization"; slugifying that would
// be unreadable, so it gets "-personal" instead.
func TestAutoLabelForPersonalOrganization(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	exp := c.now().Add(24 * time.Hour).UnixMilli()
	for token, org := range map[string][2]string{
		"at-work": {"org-work", "Threefold"},
		"at-own":  {"org-own", "info@example.com's Organization"},
	} {
		api.emails[token] = "info@example.com"
		api.orgs[token] = org
		api.usage[token] = claudeUsage
	}
	writeClaudeCreds(t, c, "at-work", "rt-work", exp)
	if _, err := c.CaptureClaudeCode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	writeClaudeCreds(t, c, "at-own", "rt-own", exp)
	saved, err := c.CaptureClaudeCode(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Record.Label != "info-personal" {
		t.Fatalf("label = %q, want info-personal", saved.Record.Label)
	}
}

func TestCodexCaptureAndSwitch(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	exp := float64(c.now().Add(24 * time.Hour).Unix())
	writeAuth := func(email, access, refresh string) {
		idToken := fakeJWT(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro", "chatgpt_account_id": "acct-" + email}})
		doc := map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "extra": "kept",
			"tokens":       map[string]any{"id_token": idToken, "access_token": fakeJWT(map[string]any{"exp": exp, "sub": access}), "refresh_token": refresh, "account_id": "acct-" + email},
			"last_refresh": c.now().UTC().Format(time.RFC3339)}
		data, _ := json.Marshal(doc)
		os.MkdirAll(c.CodexHome, 0o700)
		if err := os.WriteFile(c.codexAuthFile(), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("x@example.com", "ax", "rx")
	if _, err := c.CaptureCodex(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	advance(time.Minute)
	writeAuth("y@example.com", "ay", "ry")
	saved, err := c.CaptureCodex(ctx, "y")
	if err != nil {
		t.Fatal(err)
	}
	if TierLabel(saved.Record) != "ChatGPT Pro" {
		t.Errorf("tier = %q", TierLabel(saved.Record))
	}
	res, err := c.SwitchAccount(ctx, "codex:x", "")
	if err != nil || res.AlreadyActive {
		t.Fatalf("switch: %+v %v", res, err)
	}
	live := c.ReadCodexAuth()
	if live.LiveEmail() != "x@example.com" || live.refreshToken() != "rx" || live.Doc["extra"] != "kept" {
		t.Fatalf("auth.json after switch = %+v", live.Doc)
	}
}

func TestDeadLoginNotMaskedByCache(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	writeClaudeCreds(t, c, "at-1", "rt-1", c.now().Add(time.Hour).UnixMilli())
	api.emails["at-1"] = "a@example.com"
	api.usage["at-1"] = claudeUsage
	if _, err := c.CaptureClaudeCode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	c.Collect(ctx, CollectOptions{})
	// Claude Code logs out elsewhere; our token expires and its refresh is rejected.
	os.Remove(filepath.Join(c.ClaudeDir, ".credentials.json"))
	advance(2 * time.Hour)
	snap, _ := c.Collect(ctx, CollectOptions{})
	if r := snap.Results[0]; !r.NeedsLogin {
		t.Fatalf("expected a dead login, got %+v", r)
	}
	advance(time.Minute)
	snap, _ = c.Collect(ctx, CollectOptions{})
	if r := snap.Results[0]; !r.NeedsLogin || len(r.Usage) != 0 {
		t.Fatalf("a dead login must not be answered from the cache: %+v", r)
	}
}

func TestCallbackRejectsWrongState(t *testing.T) {
	cb, err := listenCallback(0, "expected-state", callbackPath, Claude)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d%s", cb.port, callbackPath)
	res, err := http.Get(base + "?code=abc&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("wrong state got %d", res.StatusCode)
	}
	res, err = http.Get(base + "?code=abc&state=expected-state")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	select {
	case got := <-cb.result:
		if got.err != nil || got.code != "abc" {
			t.Fatalf("callback result = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no callback result")
	}
}
