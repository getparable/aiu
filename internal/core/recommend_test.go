package core

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// limitsUsage builds a Claude usage body in the `limits` shape the API sends now.
// fable < 0 means the plan reports no Fable window at all.
func limitsUsage(session, weekly, fable float64, weeklyResetsIn time.Duration, now time.Time) json.RawMessage {
	reset := now.Add(weeklyResetsIn).UTC().Format(time.RFC3339)
	limits := []string{
		fmt.Sprintf(`{"kind":"session","group":"session","percent":%g,"resets_at":%q}`, session, now.Add(time.Hour).UTC().Format(time.RFC3339)),
		fmt.Sprintf(`{"kind":"weekly_all","group":"weekly","percent":%g,"resets_at":%q}`, weekly, reset),
	}
	if fable >= 0 {
		limits = append(limits, fmt.Sprintf(`{"kind":"weekly_scoped","group":"weekly","percent":%g,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}`, fable, reset))
	}
	body := `{"limits":[`
	for i, l := range limits {
		if i > 0 {
			body += ","
		}
		body += l
	}
	return json.RawMessage(body + `]}`)
}

func account(label, tier string, usage json.RawMessage) *Result {
	return &Result{Record: &Record{Provider: Claude, Label: label, Email: label + "@example.com", RateLimitTier: tier}, Usage: usage}
}

func TestRecommendPrefersCapacityOverPercentage(t *testing.T) {
	now := time.Now()
	// A Pro plan barely touched still carries less work than a half-spent Max 20x.
	pro := account("pro", "", limitsUsage(0, 2, 2, 5*24*time.Hour, now))
	max20 := account("max20", "default_claude_max_20x", limitsUsage(0, 50, 50, 5*24*time.Hour, now))
	pick := Recommend([]*Result{pro, max20}, now)
	if pick == nil || pick.Result != max20 {
		t.Fatalf("expected max20, got %+v", pick)
	}
	if pick.AllSpent {
		t.Error("neither account is spent")
	}
}

func TestRecommendDemotesAccountsWithoutFable(t *testing.T) {
	now := time.Now()
	// The whole point of the earlier bug: a fresh plan with no Fable at all was winning.
	noFable := account("no-fable", "", limitsUsage(7, 2, -1, 6*24*time.Hour, now))
	withFable := account("with-fable", "default_claude_max_5x", limitsUsage(16, 48, 48, 3*24*time.Hour, now))
	pick := Recommend([]*Result{noFable, withFable}, now)
	if pick == nil || pick.Result != withFable {
		t.Fatalf("expected with-fable, got %+v", pick)
	}
	// And a spent Fable window loses to one that is not, whatever the plan is.
	spentFable := account("spent-fable", "default_claude_max_20x", limitsUsage(0, 10, 100, 3*24*time.Hour, now))
	if pick := Recommend([]*Result{spentFable, withFable}, now); pick.Result != withFable {
		t.Fatalf("a spent Fable window should lose, got %+v", pick.Result.Record.Label)
	}
}

func TestRecommendSkipsExhaustedSessionWindow(t *testing.T) {
	now := time.Now()
	blocked := account("blocked", "default_claude_max_20x", limitsUsage(100, 10, 10, 3*24*time.Hour, now))
	open := account("open", "default_claude_max_5x", limitsUsage(30, 70, 70, 3*24*time.Hour, now))
	if pick := Recommend([]*Result{blocked, open}, now); pick.Result != open {
		t.Fatalf("a hit 5h limit outranks plan size, got %+v", pick.Result.Record.Label)
	}
}

func TestRecommendFallsBackToFirstResetWhenAllSpent(t *testing.T) {
	now := time.Now()
	late := account("late", "default_claude_max_20x", limitsUsage(0, 100, 100, 72*time.Hour, now))
	soon := account("soon", "", limitsUsage(0, 100, 100, 5*time.Hour, now))
	pick := Recommend([]*Result{late, soon}, now)
	if pick == nil || pick.Result != soon || !pick.AllSpent {
		t.Fatalf("expected the soonest reset with AllSpent, got %+v", pick)
	}
}

func TestRecommendIgnoresFableGateWhenNobodyHasOne(t *testing.T) {
	now := time.Now()
	codex := func(label string, pct float64) *Result {
		body := fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"allowed":true,"primary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_at":%d}}}`,
			pct, now.Add(48*time.Hour).Unix())
		return &Result{Record: &Record{Provider: Codex, Label: label, PlanType: "pro"}, Usage: json.RawMessage(body)}
	}
	a, b := codex("busy", 90), codex("free", 14)
	pick := Recommend([]*Result{a, b}, now)
	if pick.Result != b {
		t.Fatalf("expected free, got %+v", pick.Result.Record.Label)
	}
	if got := pick.Reason; got != "86% of the week left · ChatGPT Pro" {
		t.Errorf("Codex has no Fable window, so the reason must not mention one: %q", got)
	}
}

func TestRecommendSkipsUnswitchableAccounts(t *testing.T) {
	now := time.Now()
	readOnly := account("read-only", "default_claude_max_20x", limitsUsage(0, 0, 0, 5*24*time.Hour, now))
	readOnly.Record.Scopes = []string{"user:profile"}
	usable := account("usable", "default_claude_max_5x", limitsUsage(0, 60, 60, 5*24*time.Hour, now))
	if pick := Recommend([]*Result{readOnly, usable}, now); pick.Result != usable {
		t.Fatalf("a read-only login cannot be switched to, got %+v", pick.Result.Record.Label)
	}
}

func TestPlanWeight(t *testing.T) {
	for tier, want := range map[string]float64{"Max 20x": 20, "Max 5x": 5, "Pro": 1, "": 1, "ChatGPT Pro": 1} {
		if got := PlanWeight(tier); got != want {
			t.Errorf("PlanWeight(%q) = %v, want %v", tier, got, want)
		}
	}
}
