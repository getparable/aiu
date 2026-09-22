package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type cancelAfterPendingResetContext struct {
	context.Context
	c   *Config
	key string
}

type failResetTransport struct{}

func (failResetTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("synthetic transport failure")
}

func (ctx cancelAfterPendingResetContext) Err() error {
	state, err := ctx.c.loadResetState()
	if err == nil && state.Accounts[ctx.key] != nil && state.Accounts[ctx.key].Pending != nil {
		return context.Canceled
	}
	return nil
}

// A cancellation that arrives after the pending request is journaled but before
// the HTTP request starts must not strand a manual redemption that never happened.
func TestCancelledBeforeResetPostDoesNotLeavePendingRequest(t *testing.T) {
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
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := c.ConsumeBankedReset(cancelAfterPendingResetContext{Context: context.Background(), c: c, key: r.StoreKey()}, r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation before POST, got %v", err)
	}
	if posts != 0 {
		t.Fatalf("cancelled operation sent %d POSTs", posts)
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if pending := state.Accounts[r.StoreKey()].Pending; pending != nil {
		t.Fatalf("request that never reached the provider was left pending: %#v", pending)
	}
}

// A usage GET started before a reset can finish afterward. Its pre-reset payload
// must not overwrite the post-reset invalidation and appear current for five minutes.
func TestLateUsageResponseCannotRestorePreResetCache(t *testing.T) {
	usageStarted := make(chan struct{})
	allowUsageResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/usage":
			close(usageStarted)
			<-allowUsageResponse
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":100},"secondary_window":{"used_percent":100}},"rate_limit_reset_credits":{"available_count":1}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/credits/consume":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"code":"reset","windows_reset":2}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := &Config{
		Dir:                  filepath.Join(t.TempDir(), "aiu"),
		CodexUsageURL:        server.URL + "/usage",
		CodexResetCreditsURL: server.URL + "/credits",
		HTTP:                 server.Client(),
		Now:                  time.Now,
		Warn:                 func(string) {},
	}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	type fetchResult struct {
		usage *usageResult
		err   error
	}
	fetched := make(chan fetchResult, 1)
	go func() {
		u, err := c.fetchUsage(context.Background(), r, nil, nil)
		fetched <- fetchResult{u, err}
	}()
	<-usageStarted

	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	close(allowUsageResponse)
	result := <-fetched
	if result.err != nil || result.usage == nil || result.usage.usage != nil || result.usage.stale == "" {
		t.Fatalf("pre-reset response should be fenced from callers: %#v", result)
	}
	if entry := c.readCache()[r.StoreKey()]; entry != nil && entry.Usage != nil {
		t.Fatalf("pre-reset usage response repopulated invalidated cache: %s", entry.Usage)
	}
}

// Reading reset details has its own spacing slot. It must not block the first
// usage refresh for the same account.
func TestResetDetailsReadDoesNotStarveUsageFetch(t *testing.T) {
	usageGets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/credits":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"available_count":1,"credits":[],"total_earned_count":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/usage":
			usageGets++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":15},"secondary_window":{"used_percent":20}},"rate_limit_reset_credits":{"available_count":1}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := &Config{
		Dir:                  filepath.Join(t.TempDir(), "aiu"),
		CodexUsageURL:        server.URL + "/usage",
		CodexResetCreditsURL: server.URL + "/credits",
		HTTP:                 server.Client(),
		Now:                  time.Now,
		Warn:                 func(string) {},
	}
	r := resetTestAccount(t, c)
	if _, err := c.ListBankedResets(context.Background(), r.Email+"#"+r.OrgUUID, Codex); err != nil {
		t.Fatal(err)
	}
	u, err := c.fetchUsage(context.Background(), r, nil, nil)
	if err != nil || u == nil || !u.fresh || usageGets != 1 {
		t.Fatalf("details read starved usage fetch: usage=%#v err=%v GETs=%d", u, err, usageGets)
	}
}

func TestAutoResetRearmRequiresKnownWindowsButWeeklyOnlyCanTrigger(t *testing.T) {
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		a.AutoEnabled = true
		a.AutoArmed = false
		a.AutoRequestID = requestID
		a.Completed[requestID] = completedReset{Result: BankedResetRedemption{Code: "reset", RequestID: requestID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// One low reading cannot prove recovery when another reported window is
	// present but has no usable utilization value.
	c.maybeAutoReset(context.Background(), r, json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{}}}`), 1)
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	a := state.Accounts[r.StoreKey()]
	if a.AutoArmed || a.AutoRequestID != requestID {
		t.Fatalf("unknown window incorrectly rearmed automatic reset: %#v", a)
	}

	// Some accounts expose only the weekly window; a valid high weekly value
	// still counts as a threshold even when the other optional window is null.
	high, known, _ := usageResetThreshold(json.RawMessage(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":99}}}`))
	if !known || !high {
		t.Fatalf("weekly-only usage should trigger reset threshold: high=%v known=%v", high, known)
	}
}

func TestResetPost429PreservesAttemptAndHonorsCooldown(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s", r.Method)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	now := time.Now()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL, HTTP: server.Client(), Now: func() time.Time { return now }, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", requestID); err == nil {
		t.Fatal("expected the provider 429 to remain unresolved")
	}
	state, err := c.loadResetState()
	if err != nil {
		t.Fatal(err)
	}
	if pending := state.Accounts[r.StoreKey()].Pending; pending == nil || pending.RequestID != requestID {
		t.Fatalf("429 lost the in-flight idempotency key: %#v", pending)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", requestID); err == nil {
		t.Fatal("retry should report the active cooldown")
	}
	if posts != 1 {
		t.Fatalf("retry bypassed 429 cooldown: POSTs=%d", posts)
	}
}

func TestUncertainResetFailureReturnsGeneratedRetryID(t *testing.T) {
	c := &Config{
		Dir:                  filepath.Join(t.TempDir(), "aiu"),
		CodexResetCreditsURL: "https://reset.example.test/credits",
		HTTP:                 &http.Client{Transport: failResetTransport{}},
		Now:                  time.Now,
		Warn:                 func(string) {},
	}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "")
	if err == nil {
		t.Fatal("expected synthetic transport failure")
	}
	state, stateErr := c.loadResetState()
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	pending := state.Accounts[r.StoreKey()].Pending
	if pending == nil || pending.RequestID == "" {
		t.Fatalf("ambiguous attempt was not retained: %#v", pending)
	}
	if !strings.Contains(err.Error(), pending.RequestID) {
		t.Fatalf("error must include retry ID %s, got %v", pending.RequestID, err)
	}
}

func TestConcurrentConsumeAcrossConfigsUsesOneProviderPost(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s", r.Method)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":"reset","windows_reset":2}`)
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "shared")
	c1 := &Config{Dir: dir, CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c1)
	if err := c1.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A distinct Config models a second AIU process opening the same profile.
	c2 := &Config{Dir: dir, CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	results := make(chan *BankedResetRedemption, 2)
	errs := make(chan error, 2)
	call := func(c *Config) {
		out, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", requestID)
		results <- out
		errs <- err
	}
	go call(c1)
	<-started
	go call(c2)
	// Let the first durable operation complete only after the other Config has
	// had time to contend for the shared account lock.
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if out := <-results; out == nil || out.Code != "reset" || out.RequestID != requestID {
			t.Fatalf("concurrent caller did not receive the same receipt: %#v", out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("same-account callers dispatched %d provider POSTs", posts)
	}
}

func TestResetRedirectIsNotFollowed(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	if err := c.withResetState(func(s *resetState) error {
		n := 1
		resetAccount(s, r.StoreKey()).Details = &BankedResets{AvailableCount: &n}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requestID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", requestID); err == nil || !strings.Contains(err.Error(), requestID) {
		t.Fatalf("redirect should leave an unresolved receipt with its retry ID, got %v", err)
	}
	if targetHits != 0 {
		t.Fatalf("reset client followed redirect to target %d times", targetHits)
	}
}

func TestResetJournalWriteFailureFailsClosedBeforePost(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "aiu")
	c := &Config{Dir: dir, CodexResetCreditsURL: server.URL + "/credits", HTTP: server.Client(), Now: time.Now, Warn: func(string) {}}
	r := resetTestAccount(t, c)
	// A directory at the journal path makes state loading fail portably before
	// the consume request can be issued.
	if err := os.MkdirAll(c.resetStatePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeBankedReset(context.Background(), r.Email+"#"+r.OrgUUID, Codex, "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"); err == nil {
		t.Fatal("expected invalid journal path to prevent reset dispatch")
	}
	if posts != 0 {
		t.Fatalf("journal failure dispatched %d POSTs", posts)
	}
}

func TestReset429RetryAfterDateAndOverflowAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	c := &Config{Dir: filepath.Join(t.TempDir(), "aiu"), Now: func() time.Time { return now }, Warn: func(string) {}}
	date := now.Add(23 * time.Minute).Format(http.TimeFormat)
	c.recordReset429("date", &apiResponse{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{date}}})
	dateEntry := c.readCache()["date"]
	if dateEntry == nil || dateEntry.LimitedUntil < now.Add(22*time.Minute).UnixMilli() || dateEntry.LimitedUntil > now.Add(24*time.Minute).UnixMilli() {
		t.Fatalf("HTTP-date Retry-After was not honored: %#v", dateEntry)
	}
	c.recordReset429("overflow", &apiResponse{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"999999999999999999999999999999999999"}}})
	overflowEntry := c.readCache()["overflow"]
	// A positive overflow is a very long requested delay, not a malformed short
	// delay. Saturate the documented one-year bound instead of retrying in minutes.
	if overflowEntry == nil || overflowEntry.LimitedUntil != now.Add(365*24*time.Hour).UnixMilli() {
		t.Fatalf("overflowing Retry-After produced an invalid/unbounded cooldown: %#v", overflowEntry)
	}
}
