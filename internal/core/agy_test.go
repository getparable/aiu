package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAgyTestConfig(t *testing.T, now time.Time) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		Dir:             filepath.Join(dir, "aiu"),
		AgySettingsFile: filepath.Join(dir, "antigravity-cli", "settings.json"),
		Now:             func() time.Time { return now },
		Warn:            func(string) {},
		Info:            func(string) {},
	}
}

func TestIngestAgyStatusLineAndCollect(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	c := newAgyTestConfig(t, now)

	// Unauthenticated / API-key payload (no email) is a silent no-op.
	if saved, err := c.IngestAgyStatusLine([]byte(`{"product":"antigravity","agent_state":"idle"}`), ""); err != nil || saved != nil {
		t.Fatalf("expected silent no-op for empty email, got saved=%v err=%v", saved, err)
	}

	payload := []byte(`{
		"product": "antigravity",
		"email": "dev@example.com",
		"plan_tier": "Pro",
		"quota": {
			"gemini-weekly": {
				"remaining_fraction": 0.9378,
				"reset_time": "2026-11-28T07:50:32Z",
				"reset_in_seconds": 496232
			},
			"gemini-5h": {
				"remaining_fraction": 0.65,
				"reset_time": "2026-11-22T18:00:00Z",
				"reset_in_seconds": 14400
			},
			"claude-weekly": {
				"remaining_fraction": 0.5,
				"reset_time": "2026-11-28T07:50:32Z"
			},
			"ai-credits": {
				"remaining_amount": 42
			}
		}
	}`)

	saved, err := c.IngestAgyStatusLine(payload, "")
	if err != nil {
		t.Fatalf("IngestAgyStatusLine failed: %v", err)
	}
	if !saved.IsNew || saved.Record.Provider != Antigravity || saved.Record.Email != "dev@example.com" || saved.Record.Label != "dev" {
		t.Fatalf("unexpected saved account: %+v", saved)
	}
	// Passive provider must never create a token store file.
	if _, err := os.Stat(c.fileStore()); !os.IsNotExist(err) {
		t.Fatalf("passive Antigravity ingest should not create tokens.json")
	}

	// Empty-quota heartbeat (e.g. during initializing) preserves previously captured usage.
	if _, err := c.IngestAgyStatusLine([]byte(`{"product":"antigravity","email":"dev@example.com","plan_tier":"Ultra"}`), ""); err != nil {
		t.Fatalf("second ingest failed: %v", err)
	}

	snap, err := c.Collect(context.Background(), CollectOptions{Providers: []Provider{Antigravity}})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(snap.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(snap.Results))
	}
	res := snap.Results[0]
	if !res.Active || res.Err != "" || res.NeedsLogin || res.Stale != "" {
		t.Fatalf("unexpected result flags: active=%v err=%q needsLogin=%v stale=%q", res.Active, res.Err, res.NeedsLogin, res.Stale)
	}
	if !res.Record.IsReadOnly() {
		t.Fatalf("expected Antigravity record to be read-only")
	}
	if got := TierLabel(res.Record); got != "Google AI Ultra" {
		t.Fatalf("TierLabel = %q, want Google AI Ultra", got)
	}
	if health := HealthOf(res, now); health.State != "ok" {
		t.Fatalf("HealthOf = %+v, want ok", health)
	}

	ws := NormalizeWindows(res.Usage, now)
	if len(ws) != 3 {
		t.Fatalf("expected 3 percentage windows (excluding ai-credits), got %d: %+v", len(ws), ws)
	}
	if ws[0].Group != "session" || ws[0].Label != "5h Gemini" || ws[0].Percent != 35 {
		t.Fatalf("unexpected first window: %+v", ws[0])
	}
	if ws[1].Group != "weekly" || ws[1].Label != "7d Gemini" || ws[1].Percent != 6.22 {
		t.Fatalf("unexpected second window: %+v", ws[1])
	}
	if ws[2].Group != "weekly" || ws[2].Label != "7d Claude" || ws[2].Percent != 50 {
		t.Fatalf("unexpected third window: %+v", ws[2])
	}

	credits := AgyCreditsOf(res.Usage)
	if len(credits) != 1 || credits[0].Label != "AI credits" || credits[0].Remaining != 42 {
		t.Fatalf("unexpected credits: %+v", credits)
	}

	// Advance time past the 5h reset_time: that window rolls over to 0% and Collect marks stale.
	later := now.Add(5 * time.Hour)
	c.Now = func() time.Time { return later }
	snap2, err := c.Collect(context.Background(), CollectOptions{Providers: []Provider{Antigravity}})
	if err != nil {
		t.Fatalf("Collect after rollover failed: %v", err)
	}
	if !strings.Contains(snap2.Results[0].Stale, "window reset") {
		t.Fatalf("expected rollover stale note, got %q", snap2.Results[0].Stale)
	}
	ws2 := NormalizeWindows(snap2.Results[0].Usage, later)
	if ws2[0].Percent != 0 || ws2[0].ResetsAt != "" {
		t.Fatalf("expected rolled-over 5h window to show 0%% with empty resetsAt, got %+v", ws2[0])
	}
	if ws2[1].Percent != 6.22 {
		t.Fatalf("expected weekly window to keep 6.22%%, got %+v", ws2[1])
	}
}

func TestAgyPassiveGuardsAndLookup(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	c := newAgyTestConfig(t, now)

	_, err := c.IngestAgyStatusLine([]byte(`{
		"email": "personal@gmail.com",
		"plan_tier": "Pro",
		"quota": {"gemini-5h": {"remaining_fraction": 0.2, "reset_in_seconds": 3600, "disabled": true}}
	}`), "personal")
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	// agy: and antigravity: prefixes both resolve.
	for _, sel := range []string{"agy:personal", "antigravity:personal@gmail.com", "agy:personal@gmail.com#"} {
		if _, entry, err := c.FindAccount(sel, ""); err != nil || entry.Provider != Antigravity {
			t.Fatalf("FindAccount(%q) = %+v, %v", sel, entry, err)
		}
	}

	// Login and switch are refused cleanly (ToS-safe read-only provider).
	if _, err := c.BeginLogin(LoginOptions{Provider: Antigravity}); err == nil || !strings.Contains(err.Error(), "statusLine") {
		t.Fatalf("expected BeginLogin refusal, got %v", err)
	}
	if _, err := c.SwitchAccount(context.Background(), "agy:personal", ""); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected SwitchAccount refusal, got %v", err)
	}

	// Disabled bucket reports severity "locked" and reset_in_seconds derives resetsAt.
	snap, err := c.Collect(context.Background(), CollectOptions{Providers: []Provider{Antigravity}})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	ws := NormalizeWindows(snap.Results[0].Usage, now)
	if len(ws) != 1 || ws[0].Severity != "locked" || ws[0].ResetsAt != "2026-11-22T15:00:00Z" {
		t.Fatalf("unexpected disabled/reset_in_seconds window: %+v", ws)
	}

	// Removing cleans index, cache, and active marker.
	if _, err := c.RemoveAccount("agy:personal", ""); err != nil {
		t.Fatalf("RemoveAccount failed: %v", err)
	}
	if got := c.AgyActiveEmail(); got != "" {
		t.Fatalf("expected active email cleared after removal, got %q", got)
	}
}

func TestSetupAgyStatusLine(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	c := newAgyTestConfig(t, now)

	// Preserve existing user settings when installing the hook.
	if err := os.MkdirAll(filepath.Dir(c.AgySettingsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := "{\n  \"colorScheme\": \"tokyo night\",\n  \"allowNonWorkspaceAccess\": true\n}\n"
	if err := os.WriteFile(c.AgySettingsFile, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := c.SetupAgyStatusLine("~/.local/bin/aiu")
	if err != nil {
		t.Fatalf("SetupAgyStatusLine failed: %v", err)
	}
	if !st.Configured || st.AlreadyConfigured || st.CustomExisting {
		t.Fatalf("unexpected status on first setup: %+v", st)
	}

	raw, err := os.ReadFile(c.AgySettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid settings.json written: %v", err)
	}
	if doc["colorScheme"] != "tokyo night" || doc["allowNonWorkspaceAccess"] != true {
		t.Fatalf("existing settings keys lost: %+v", doc)
	}
	sl := obj(doc["statusLine"])
	if sl["type"] != "command" || sl["command"] != "~/.local/bin/aiu hook agy" || sl["stack_with_default"] != true {
		t.Fatalf("unexpected statusLine block: %+v", sl)
	}

	// Re-running is idempotent.
	st2, err := c.SetupAgyStatusLine("~/.local/bin/aiu")
	if err != nil || !st2.Configured || !st2.AlreadyConfigured {
		t.Fatalf("expected AlreadyConfigured on repeat setup, got %+v, %v", st2, err)
	}

	// Custom non-AIU statusLine is never clobbered.
	custom := "{\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"~/.gemini/antigravity-cli/custom.sh\"\n  }\n}\n"
	if err := os.WriteFile(c.AgySettingsFile, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}
	st3, err := c.SetupAgyStatusLine("~/.local/bin/aiu")
	if err != nil || !st3.CustomExisting || st3.Configured {
		t.Fatalf("expected CustomExisting without overwrite, got %+v, %v", st3, err)
	}
	after, _ := os.ReadFile(c.AgySettingsFile)
	if string(after) != custom {
		t.Fatalf("custom settings.json was modified:\n%s", string(after))
	}
}
