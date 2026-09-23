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
	"strconv"
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
			if _, err := io.WriteString(w, `{"available_count":3,"credits":[{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-09-01T00:00:00Z","expires_at":null,"title":null,"description":null}],"total_earned_count":5}`); err != nil {
				t.Error(err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/credits/consume":
			posts++
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["redeem_request_id"] != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || payload["credit_id"] != "credit-1" {
				t.Errorf("wrong payload: %#v", payload)
			}
			if _, err := io.WriteString(w, `{"code":"reset","windows_reset":2}`); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	threshold := 7
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &threshold); err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListBankedResets(context.Background(), r.Email+"#"+r.OrgUUID, Codex)
	if err != nil {
		t.Fatal(err)
	}
	if listed.AvailableCount == nil || *listed.AvailableCount != 3 || len(listed.Credits) != 1 || !listed.Credits[0].CanRedeem || listed.AutoResetThresholdPercent != 7 {
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

func TestListBankedResetsDoesNotOfferSpendWhilePending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected request %s", r.Method)
		}
		if _, err := io.WriteString(w, `{"available_count":1,"credits":[]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if err := c.withResetState(func(s *resetState) error {
		resetAccount(s, r.StoreKey()).Pending = &BankedResetRequest{RequestID: requestID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := c.ListBankedResets(context.Background(), r.Email+"#"+r.OrgUUID, Codex)
	if err != nil {
		t.Fatal(err)
	}
	if view.AvailableCount == nil || *view.AvailableCount != 1 || view.PendingRequest == nil || view.PendingRequest.RequestID != requestID || view.CanRedeem {
		t.Fatalf("fresh details offered a second spend while a request is pending: %+v", view)
	}
}

func TestListBankedResetsDoesNotOfferSpendAfter429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := c.ListBankedResets(context.Background(), r.Email+"#"+r.OrgUUID, Codex)
	if err != nil {
		t.Fatal(err)
	}
	if view.AvailableCount == nil || *view.AvailableCount != 1 || view.CanRedeem {
		t.Fatalf("429 response offered a spend during provider cooldown: %+v", view)
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
		if _, err := io.WriteString(w, `{"code":"reset","windows_reset":1}`); err != nil {
			t.Error(err)
		}
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		cancel()
		if _, err := io.WriteString(w, `{"code":"reset"}`); err != nil {
			t.Error(err)
		}
	}))
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

func TestAutoResetThresholdValidationDefaultsAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int
		used      float64
		want      bool
	}{{"zero exact", 0, 100, true}, {"zero below", 0, 99.99, false}, {"five exact", 5, 95, true}, {"ten exact", 10, 90, true}, {"ninety-nine exact", 99, 1, true}, {"five below", 5, 94.99, false}} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": tc.used}, "secondary_window": nil}})
			high, known, _ := usageThreshold(raw, tc.threshold, tc.threshold)
			if !known || high != tc.want {
				t.Fatalf("threshold %d at used %.2f => high=%v known=%v", tc.threshold, tc.used, high, known)
			}
		})
	}
	dir := t.TempDir()
	c := &Config{Dir: filepath.Join(dir, "aiu"), Now: time.Now, Warn: func(string) {}}
	if err := c.ConfigureAutoReset(context.Background(), "anything", Codex, nil, nil); err == nil {
		t.Fatal("empty partial settings should fail")
	}
	bad := 100
	if err := c.ConfigureAutoReset(context.Background(), "anything", Codex, nil, &bad); err == nil {
		t.Fatal("out of range must fail before account lookup")
	}
	if _, err := os.Stat(c.resetStatePath()); !os.IsNotExist(err) {
		t.Fatalf("invalid configuration mutated storage: %v", err)
	}
	r := resetTestAccount(t, c)
	if got := c.cachedBankedResets(r, nil, "").AutoResetThresholdPercent; got != 1 {
		t.Fatalf("legacy missing threshold default=%d", got)
	}
	zero := 0
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &zero); err != nil {
		t.Fatal(err)
	}
	if got := c.cachedBankedResets(r, nil, "").AutoResetThresholdPercent; got != 0 {
		t.Fatalf("explicit zero should be preserved, got %d", got)
	}
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, true); err != nil {
		t.Fatal(err)
	}
	if got := c.cachedBankedResets(r, nil, "").AutoResetThresholdPercent; got != 0 {
		t.Fatalf("SetAutoReset reset saved threshold: %d", got)
	}
	r2 := &Record{Provider: Codex, Email: "other@example.test", OrgUUID: "acct-2", AccountID: "acct-2", AccessToken: "synthetic-token-2", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	idx, err := c.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.Accounts = append(idx.Accounts, &IndexEntry{Email: r2.Email, Label: "other", Org: r2.OrgUUID, Provider: Codex})
	if err := c.saveIndex(idx); err != nil {
		t.Fatal(err)
	}
	if err := c.tokenSet(r2.StoreKey(), r2); err != nil {
		t.Fatal(err)
	}
	ten := 10
	if err := c.ConfigureAutoReset(context.Background(), r2.Email+"#"+r2.OrgUUID, Codex, nil, &ten); err != nil {
		t.Fatal(err)
	}
	if got := c.cachedBankedResets(r2, nil, "").AutoResetThresholdPercent; got != 10 {
		t.Fatalf("second account threshold=%d", got)
	}
	if got := c.cachedBankedResets(r, nil, "").AutoResetThresholdPercent; got != 0 {
		t.Fatalf("second account changed first threshold to %d", got)
	}
}

func TestThresholdChangesKeepPendingAndOriginalRetryThreshold(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if posts == 1 {
			w.WriteHeader(500)
			return
		}
		if _, err := io.WriteString(w, `{"code":"reset"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	one := 1
	five := 5
	enabled := true
	if err := c.withResetState(func(s *resetState) error {
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &one}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, &enabled, &five); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", id); err == nil {
		t.Fatal("first synthetic response should be uncertain")
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a := state.Accounts[r.StoreKey()]
	if a.Pending == nil || a.PendingThresholdPercent == nil || *a.PendingThresholdPercent != 5 {
		t.Fatalf("pending threshold not captured: %#v", a)
	}
	twenty := 20
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &twenty); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", id); err != nil {
		t.Fatal(err)
	}
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a = state.Accounts[r.StoreKey()]
	if a.Pending != nil || a.AutoRequestID != id || a.LastResetThresholdPercent == nil || *a.LastResetThresholdPercent != 5 || a.Completed[id].ThresholdPercent == nil || *a.Completed[id].ThresholdPercent != 5 {
		t.Fatalf("retry changed the original threshold/latch: %#v", a)
	}
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, true); err != nil {
		t.Fatal(err)
	}
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a = state.Accounts[r.StoreKey()]
	if a.AutoRequestID != id || a.AutoArmed {
		t.Fatalf("off/on bypassed spent latch: %#v", a)
	}
	one = 1
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &one); err != nil {
		t.Fatal(err)
	}
	// Lowering the threshold cannot let an observation with only 3% remaining
	// rearm a reset that was issued at a 5% threshold.
	usage := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":97},"secondary_window":{"used_percent":97}},"rate_limit_reset_credits":{"available_count":1}}`)
	c.maybeAutoReset(context.Background(), r, usage, time.Now().Add(time.Second).UnixMilli())
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Accounts[r.StoreKey()].AutoRequestID != id {
		t.Fatal("lowering threshold bypassed the original spend latch")
	}
	// Current threshold is 20, original attempt was 5: 11% remaining is not above both.
	twenty = 20
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &twenty); err != nil {
		t.Fatal(err)
	}
	usage = json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":89},"secondary_window":{"used_percent":89}},"rate_limit_reset_credits":{"available_count":1}}`)
	c.maybeAutoReset(context.Background(), r, usage, time.Now().Add(time.Second).UnixMilli())
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Accounts[r.StoreKey()].AutoRequestID != id {
		t.Fatal("lower recovery did not preserve latch")
	}
	usage = json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"used_percent":0}},"rate_limit_reset_credits":{"available_count":1}}`)
	c.maybeAutoReset(context.Background(), r, usage, time.Now().Add(2*time.Second).UnixMilli())
	state, err = c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Accounts[r.StoreKey()].AutoRequestID != "" || !state.Accounts[r.StoreKey()].AutoArmed {
		t.Fatal("above both thresholds did not rearm")
	}
}

func TestAutoResetConfiguredThresholdControlsActualPOSTBoundary(t *testing.T) {
	for _, threshold := range []int{0, 5, 10, 99} {
		t.Run(strconv.Itoa(threshold), func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/consume" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				posts++
				if _, err := io.WriteString(w, `{"code":"reset"}`); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
			r := resetTestAccount(t, c)
			enabled := true
			if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, &enabled, &threshold); err != nil {
				t.Fatal(err)
			}
			used := float64(100-threshold) - .01
			below, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": used}, "secondary_window": nil}, "rate_limit_reset_credits": map[string]any{"available_count": 2}})
			c.maybeAutoReset(context.Background(), r, below, time.Now().UnixMilli())
			if posts != 0 {
				t.Fatalf("below boundary issued %d requests", posts)
			}
			at, _ := json.Marshal(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": float64(100 - threshold)}, "secondary_window": nil}, "rate_limit_reset_credits": map[string]any{"available_count": 2}})
			c.maybeAutoReset(context.Background(), r, at, time.Now().Add(time.Second).UnixMilli())
			if posts != 1 {
				t.Fatalf("exact inclusive boundary issued %d requests", posts)
			}
		})
	}
}

func TestRecoveredAutoIntentKeepsThresholdCapturedBeforeCrash(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if _, err := io.WriteString(w, `{"code":"reset"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	five, twenty := 5, 20
	one := 1
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		a.AutoEnabled = true
		a.AutoArmed = false
		a.AutoRequestID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		a.AutoRequestThresholdPercent = &five // durable auto intent, before pending dispatch
		a.Details = &BankedResets{AvailableCount: &one}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &twenty); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a := state.Accounts[r.StoreKey()]
	if posts != 1 || a.LastResetThresholdPercent == nil || *a.LastResetThresholdPercent != 5 || a.Completed[a.AutoRequestID].ThresholdPercent == nil || *a.Completed[a.AutoRequestID].ThresholdPercent != 5 {
		t.Fatalf("recovered intent used edited threshold: posts=%d account=%#v", posts, a)
	}
}

func TestLegacyAutoIntentUsesDefaultThresholdAfterPreferenceEdit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"code":"reset"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	one := 1
	twenty := 0
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		a.AutoEnabled, a.AutoArmed = true, false
		a.AutoRequestID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		a.Details = &BankedResets{AvailableCount: &one}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureAutoReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, nil, &twenty); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Accounts[r.StoreKey()].Completed["aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"].ThresholdPercent; got == nil || *got != 1 {
		t.Fatalf("legacy auto intent should retain default threshold 1: %v", got)
	}
}
