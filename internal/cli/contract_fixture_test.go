package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/getparable/aiu/internal/core"
)

// These fixtures are also decoded by the actual Swift and Rust presentation
// models. Updating them requires reviewing compatibility with both frontends.
func TestSharedAccountContract(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &core.Snapshot{Results: []*core.Result{
		{Record: &core.Record{Provider: core.Claude, Email: "work@example.test", Label: "Work", OrgUUID: "org-work", OrgName: "Example Team", SubscriptionType: "max", Scopes: []string{"user:inference"}, ExpiresAt: now.Add(time.Hour).UnixMilli()}, Active: true, FetchedAt: now.UnixMilli(), Usage: json.RawMessage(`{"five_hour":{"utilization":12,"resets_at":"2030-01-01T05:00:00Z"},"seven_day":{"utilization":23,"resets_at":"2030-01-07T00:00:00Z"}}`)},
		{Record: &core.Record{Provider: core.Codex, Email: "personal@example.test", Label: "Personal", OrgUUID: "org-personal", PlanType: "plus", Missing: true}, Err: "no stored token", NeedsLogin: true},
		{Record: &core.Record{Provider: core.Claude, Email: "view@example.test", Label: "Read only", Scopes: []string{"user:profile"}}, Stale: "showing the last known values", FetchedAt: now.Add(-time.Hour).UnixMilli(), Usage: json.RawMessage(`{"five_hour":{"utilization":null,"locked_reason":"restricted"},"seven_day":{"utilization":75}}`)},
	}}
	for name, value := range map[string][]jsonAccount{"accounts.json": toJSON(snap, now), "empty.json": toJSON(&core.Snapshot{Empty: true}, now)} {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("..", "..", "tests", "fixtures", "frontend", name)
		if os.Getenv("AIU_UPDATE_CONTRACT_FIXTURES") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(data, &gotValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("%s no longer matches the shared frontend contract", name)
		}
	}
}

func TestSharedTerminalContract(t *testing.T) {
	for name, value := range map[string]frontendEvent{
		"cancelled.json": {Version: 1, Event: "result", Cancelled: true, Error: &frontendError{Code: "cancelled", Message: "frontend command cancelled"}},
		"failure.json":   {Version: 1, Event: "result", Error: &frontendError{Code: "operation_failed", Message: "fixture operation failed"}},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "frontend", name))
		if err != nil {
			t.Fatal(err)
		}
		var fixture frontendEvent
		if err := json.Unmarshal(data, &fixture); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fixture, value) {
			t.Fatalf("%s no longer matches terminal contract", name)
		}
	}
}
