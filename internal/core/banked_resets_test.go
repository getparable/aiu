package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetTestAccount(t *testing.T, c *Config) *Record {
	t.Helper()
	r := &Record{Provider: Codex, Email: "reset@example.test", OrgUUID: "acct-1", AccountID: "acct-1", AccessToken: "synthetic-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	idx := &Index{Version: 1, Accounts: []*IndexEntry{{Email: r.Email, Label: "reset", Org: r.OrgUUID, Provider: Codex}}}
	if err := c.saveIndex(idx); err != nil {
		t.Fatal(err)
	}
	if err := c.tokenSet(r.StoreKey(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestListAndConsumeBankedResetSyntheticAPI(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			t.Errorf("missing auth/account headers: %#v", r.Header)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/credits":
			io.WriteString(w, `{"available_count":3,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z","expires_at":null,"title":null,"description":null}],"total_earned_count":5}`)
		case r.Method == http.MethodPost && r.URL.Path == "/credits/consume":
			posts++
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["redeem_request_id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || payload["credit_id"] != "credit-1" {
				t.Errorf("wrong payload: %#v", payload)
			}
			io.WriteString(w, `{"code":"reset","windows_reset":2}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	listed, err := c.ListBankedResets(context.Background(), r.Email+"#"+r.OrgUUID, Codex)
	if err != nil {
		t.Fatal(err)
	}
	if listed.AvailableCount == nil || *listed.AvailableCount != 3 || len(listed.Credits) != 1 || !listed.Credits[0].CanRedeem {
		t.Fatalf("unexpected list: %#v", listed)
	}
	request := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	first, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "credit-1", request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "credit-1", request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Code != "reset" || first.WindowsReset != 2 || second.Code != first.Code || posts != 1 {
		t.Fatalf("result=%#v cached=%#v posts=%d", first, second, posts)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "different-credit", request); err == nil || !strings.Contains(err.Error(), "different credit") {
		t.Fatalf("expected reused request ID conflict, got %v", err)
	}
}

func TestConsumeBankedResetNeedsPersistedAvailableCredit(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(500)
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	_, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "nope", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err == nil || posts != 0 {
		t.Fatalf("must reject before POST, err=%v posts=%d", err, posts)
	}
}

func TestAutoResetPreferenceAndThresholdUnknown(t *testing.T) {
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	c.maybeAutoReset(context.Background(), r, json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":99.1},"secondary_window":{"used_percent":22}},"rate_limit_reset_credits":{"available_count":4}}`), time.Now().UnixMilli())
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if a := state.Accounts[r.StoreKey()]; a != nil && a.AutoEnabled {
		t.Fatal("automatic resets must default off")
	}
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, true); err != nil {
		t.Fatal(err)
	}
	c.maybeAutoReset(context.Background(), r, json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":98.9},"secondary_window":{"used_percent":20}},"rate_limit_reset_credits":{"available_count":4}}`), time.Now().Add(time.Second).UnixMilli())
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if a := state.Accounts[r.StoreKey()]; a == nil || !a.AutoEnabled || !a.AutoArmed {
		t.Fatalf("below threshold should remain armed: %#v", a)
	}
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.resetStatePath()); err != nil {
		t.Fatal(err)
	}
	if high, known, _ := usageResetThreshold(json.RawMessage(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":99}}}`)); !known || !high {
		t.Fatalf("weekly-only usage should trigger: high=%v known=%v", high, known)
	}
	if high, known, _ := usageResetThreshold(json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":98.9},"secondary_window":null}}`)); !known || high {
		t.Fatalf("partial below-threshold usage should be known: high=%v known=%v", high, known)
	}
}

func TestAutoResetUsesProviderSelectedCreditAndLatches(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s", r.Method)
			w.WriteHeader(404)
			return
		}
		posts++
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["credit_id"] != "" {
			t.Errorf("auto reset must let provider select credit: %#v", payload)
		}
		io.WriteString(w, `{"code":"reset","windows_reset":1}`)
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, true); err != nil {
		t.Fatal(err)
	}
	high := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":99},"secondary_window":null},"rate_limit_reset_credits":{"available_count":2}}`)
	c.maybeAutoReset(context.Background(), r, high, time.Now().UnixMilli())
	c.maybeAutoReset(context.Background(), r, high, time.Now().Add(time.Second).UnixMilli())
	if posts != 1 {
		t.Fatalf("high threshold should spend once until recovery, got %d posts", posts)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a := state.Accounts[r.StoreKey()]
	if a == nil || a.AutoArmed || a.AutoRequestID == "" {
		t.Fatalf("missing spend latch: %#v", a)
	}
	low := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":null},"rate_limit_reset_credits":{"available_count":2}}`)
	c.maybeAutoReset(context.Background(), r, low, time.Now().Add(2*time.Second).UnixMilli())
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a = state.Accounts[r.StoreKey()]
	if a == nil || !a.AutoArmed || a.AutoRequestID != "" {
		t.Fatalf("fresh recovered usage should rearm: %#v", a)
	}
}

func TestConsumePersistsOutcomeAfterRequestCancelsCaller(t *testing.T) {
	var cancel context.CancelFunc
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; cancel(); io.WriteString(w, `{"code":"reset"}`) }))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		n := 1
		a.Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	result, err := c.ConsumeBankedReset(ctx, r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil || result == nil || result.Code != "reset" || posts != 1 {
		t.Fatalf("result=%#v err=%v posts=%d", result, err, posts)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Accounts[r.StoreKey()].Completed[result.RequestID]; !ok {
		t.Fatal("definitive response was not journaled")
	}
}

func TestCancelledRetryKeepsPreviouslyIssuedPendingRequest(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		n := 1
		a.Details = &BankedResets{AvailableCount: &n}
		a.Pending = &BankedResetRequest{RequestID: requestID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.ConsumeBankedReset(cancelAfterPendingResetContext{Context: context.Background(), c: c, key: r.StoreKey()}, r.Email+"#"+r.OrgUUID, Codex, "", requestID)
	if !errors.Is(err, context.Canceled) || posts != 0 {
		t.Fatalf("err=%v posts=%d", err, posts)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	pending := state.Accounts[r.StoreKey()].Pending
	if pending == nil || pending.RequestID != requestID {
		t.Fatalf("cancelled retry erased previously issued intent: %#v", pending)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"); err == nil {
		t.Fatal("a new request ID must remain blocked")
	}
}
