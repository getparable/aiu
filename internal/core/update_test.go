package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.1", -1},
		{"0.1.1", "0.1.0", 1},
		{"0.1.1", "0.1.1", 0},
		{"v0.1.1", "0.1.1", 0},     // the tags carry a v, the binary does not
		{"0.2.0", "0.10.0", -1},    // dotted fields are numbers, not text
		{"1.0", "1.0.0", 0},        // a missing field is zero
		{"0.2.0-rc.1", "0.2.0", 0}, // a pre-release suffix is not a version difference
		{"dev", "0.1.1", 0},        // unparseable never prompts an upgrade
		{"0.1.1", "", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsDevelopmentVersion(t *testing.T) {
	for _, v := range []string{"", "dev", "0.1.1-dirty"} {
		if !IsDevelopmentVersion(v) {
			t.Errorf("IsDevelopmentVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"0.1.1", "v0.2.0"} {
		if IsDevelopmentVersion(v) {
			t.Errorf("IsDevelopmentVersion(%q) = true, want false", v)
		}
	}
}

func TestUpgradeCommandFollowsInstallSource(t *testing.T) {
	if got := UpgradeCommand(SourceHomebrew); got != brewUpgradeCommand {
		t.Errorf("homebrew: got %q", got)
	}
	if got := UpgradeCommand(SourceBuilt); got != sourceUpgradeCommand {
		t.Errorf("built: got %q", got)
	}
	// Nothing sensible to suggest beats suggesting the wrong thing.
	if got := UpgradeCommand(SourceUnknown); got != "" {
		t.Errorf("unknown: got %q, want empty", got)
	}
}

// releaseServer serves one /releases/latest answer and counts how often it is asked.
type releaseServer struct {
	t    *testing.T
	tag  string
	code int
	hits int
	body string
	srv  *httptest.Server
}

func newReleaseServer(t *testing.T, tag string) *releaseServer {
	t.Helper()
	r := &releaseServer{t: t, tag: tag, code: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.hits++
		if r.code != http.StatusOK {
			w.WriteHeader(r.code)
			return
		}
		body := r.body
		if body == "" {
			body = `{"tag_name":"` + r.tag + `","html_url":"https://example.test/releases/` + r.tag + `"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// updateConfig builds a Config pointed at the fake release server, with a clock the
// test can move and a fresh cache directory.
func updateConfig(t *testing.T, r *releaseServer, version string) (*Config, func(time.Duration)) {
	t.Helper()
	old := Version
	Version = version
	t.Cleanup(func() { Version = old })

	now := time.UnixMilli(1_789_560_000_000).UTC()
	c := &Config{
		Dir:           t.TempDir(),
		ReleaseAPIURL: r.srv.URL + "/releases/latest",
		HTTP:          r.srv.Client(),
		Warn:          func(string) {},
		Info:          func(string) {},
	}
	c.Now = func() time.Time { return now }
	return c, func(d time.Duration) { now = now.Add(d) }
}

func TestCheckUpdateReportsAvailable(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "0.1.1")

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "available" {
		t.Fatalf("state = %q, want available (detail: %q, error: %q)", got.State, got.Detail, got.Error)
	}
	if !got.Available() {
		t.Error("Available() = false")
	}
	if got.Latest != "v0.2.0" {
		t.Errorf("latest = %q", got.Latest)
	}
	// The detail is what a person reads; it should carry both versions without the v.
	if want := "0.2.0 is available — this is 0.1.1"; got.Detail != want {
		t.Errorf("detail = %q, want %q", got.Detail, want)
	}
}

func TestCheckUpdateReportsCurrentWhenAhead(t *testing.T) {
	r := newReleaseServer(t, "v0.1.1")
	c, _ := updateConfig(t, r, "0.2.0") // a local build ahead of the last release

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "current" {
		t.Fatalf("state = %q, want current", got.State)
	}
	if got.Available() {
		t.Error("Available() = true for a build ahead of the release")
	}
}

func TestCheckUpdateUsesCacheUntilIntervalPasses(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, advance := updateConfig(t, r, "0.1.1")

	first := c.CheckUpdate(context.Background(), false)
	if first.Cached {
		t.Error("first check reported as cached")
	}
	if r.hits != 1 {
		t.Fatalf("hits after first check = %d, want 1", r.hits)
	}

	second := c.CheckUpdate(context.Background(), false)
	if !second.Cached {
		t.Error("second check should have come from the cache")
	}
	if r.hits != 1 {
		t.Errorf("hits after cached check = %d, want 1 — GitHub was asked twice", r.hits)
	}
	if second.State != first.State || second.Latest != first.Latest {
		t.Errorf("cached answer differs: %+v vs %+v", second, first)
	}

	advance(UpdateCheckInterval + time.Minute)
	third := c.CheckUpdate(context.Background(), false)
	if third.Cached {
		t.Error("check after the interval should have refetched")
	}
	if r.hits != 2 {
		t.Errorf("hits after interval = %d, want 2", r.hits)
	}
}

func TestCheckUpdateForceBypassesCache(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "0.1.1")

	c.CheckUpdate(context.Background(), false)
	c.CheckUpdate(context.Background(), true)
	if r.hits != 2 {
		t.Errorf("hits = %d, want 2 — --force did not refetch", r.hits)
	}
}

// The cache holds what GitHub said, never the verdict: after an upgrade lands while the
// cache is still warm, the same cached tag has to read as "current" rather than keep
// advertising the version the user just installed.
func TestCheckUpdateRecomputesVerdictAgainstRunningVersion(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "0.1.1")

	if got := c.CheckUpdate(context.Background(), false); got.State != "available" {
		t.Fatalf("before upgrade: state = %q, want available", got.State)
	}

	Version = "0.2.0" // the upgrade lands; the cache is untouched and still fresh
	got := c.CheckUpdate(context.Background(), false)
	if !got.Cached {
		t.Fatal("expected the warm cache to be reused")
	}
	if got.State != "current" {
		t.Errorf("after upgrade: state = %q, want current", got.State)
	}
}

func TestCheckUpdateKeepsStaleAnswerWhenGitHubFails(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, advance := updateConfig(t, r, "0.1.1")

	c.CheckUpdate(context.Background(), false)
	advance(UpdateCheckInterval + time.Minute)
	r.code = http.StatusForbidden // rate limited

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "available" {
		t.Errorf("state = %q, want the stale available answer to survive", got.State)
	}
	if got.Error == "" {
		t.Error("the failure should still be reported alongside the stale answer")
	}
	if !got.Cached {
		t.Error("a stale answer should be marked cached")
	}
}

func TestCheckUpdateWithoutCacheReportsUnknown(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "0.1.1")
	r.code = http.StatusNotFound // tags pushed, but no release published yet

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "unknown" {
		t.Errorf("state = %q, want unknown", got.State)
	}
	if got.Available() {
		t.Error("a failed check must not claim an update is available")
	}
	if got.Error == "" {
		t.Error("want the reason recorded")
	}
}

func TestCheckUpdateDevelopmentBuildNeverAsksGitHub(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "dev")

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "development" {
		t.Errorf("state = %q, want development", got.State)
	}
	if r.hits != 0 {
		t.Errorf("hits = %d, want 0 — a dev build should not call GitHub", r.hits)
	}
}

func TestCheckUpdateRejectsDraftAndPrerelease(t *testing.T) {
	r := newReleaseServer(t, "v0.2.0")
	c, _ := updateConfig(t, r, "0.1.1")
	r.body = `{"tag_name":"v0.2.0","prerelease":true}`

	got := c.CheckUpdate(context.Background(), false)
	if got.State != "unknown" {
		t.Errorf("state = %q, want unknown for a prerelease", got.State)
	}
}
