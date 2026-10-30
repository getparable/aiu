package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexSessionEndedRequiresRelink(t *testing.T) {
	api := newFakeAPI(t)
	c := testConfig(t, api)
	ctx := context.Background()
	r := &Record{
		Provider: Codex, Email: "work@example.test", OrgUUID: "work-org",
		AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: c.now().Add(time.Hour).UnixMilli(), LastRefresh: c.now().UnixMilli(),
	}
	if err := c.tokenSet(r.StoreKey(), r); err != nil {
		t.Fatal(err)
	}
	if err := c.saveIndex(&Index{Version: 1, Accounts: []*IndexEntry{{Provider: Codex, Email: r.Email, Org: r.OrgUUID}}}); err != nil {
		t.Fatal(err)
	}
	api.usage[r.AccessToken] = codexUsage
	collect := func() *Result {
		t.Helper()
		snap, err := c.Collect(ctx, CollectOptions{Providers: []Provider{Codex}})
		if err != nil || len(snap.Results) != 1 {
			t.Fatalf("collect: err=%v", err)
		}
		return snap.Results[0]
	}
	if got := collect(); got.Err != "" || len(got.Usage) == 0 {
		t.Fatalf("initial usage: error=%q", got.Err)
	}

	var refreshes atomic.Int32
	var refreshStatus atomic.Int32
	refreshStatus.Store(http.StatusServiceUnavailable)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		status := int(refreshStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusUnauthorized {
			fmt.Fprint(w, `{"error":{"message":"Your session has ended. Please log in again."}}`)
		} else {
			fmt.Fprint(w, `{"error":{"message":"Service temporarily unavailable."}}`)
		}
	}))
	t.Cleanup(tokens.Close)
	c.CodexTokenURL = tokens.URL
	api.status[r.AccessToken] = http.StatusUnauthorized
	advance(MinFetchSpacing)
	if got := collect(); got.Err == "" || got.NeedsLogin || HealthOf(got, c.now()).State != "ok" {
		t.Fatalf("temporary refresh failure requires relink: needsLogin=%v error=%q", got.NeedsLogin, got.Err)
	}
	refreshStatus.Store(http.StatusUnauthorized)
	advance(MinFetchSpacing)
	for _, phase := range []string{"rejected refresh", "cached rejection"} {
		got := collect()
		if !got.NeedsLogin || HealthOf(got, c.now()).State != "expired" || len(got.Usage) != 0 {
			t.Errorf("%s: needsLogin=%v health=%s hasUsage=%v error=%q stale=%q", phase, got.NeedsLogin, HealthOf(got, c.now()).State, len(got.Usage) > 0, got.Err, got.Stale)
		}
	}
	if refreshes.Load() != 2 || api.usageHit.Load() != 3 {
		t.Fatalf("cached rejection retried network: refreshes=%d usage=%d", refreshes.Load(), api.usageHit.Load())
	}

	// New credentials must recover immediately, even inside the fetch spacing.
	advance(time.Second)
	r.AccessToken, r.RefreshToken = "new-access", "new-refresh"
	r.UpdatedAt = c.now().UnixMilli()
	if err := c.tokenSet(r.StoreKey(), r); err != nil {
		t.Fatal(err)
	}
	api.usage[r.AccessToken] = codexUsage
	if got := collect(); got.Err != "" || got.NeedsLogin || len(got.Usage) == 0 || got.Stale != "" || got.FetchedAt != c.now().UnixMilli() {
		t.Fatalf("renewed login did not recover: needsLogin=%v hasUsage=%v error=%q", got.NeedsLogin, len(got.Usage) > 0, got.Err)
	}
	if api.usageHit.Load() != 4 {
		t.Fatalf("renewed login did not fetch new usage: hits=%d", api.usageHit.Load())
	}
}
