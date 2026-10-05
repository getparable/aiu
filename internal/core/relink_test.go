package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserRelink(t *testing.T) {
	for _, provider := range []Provider{Claude, Codex} {
		for _, scenario := range []string{"same account", "wrong email", "wrong organization", "removed during login", "missing credentials"} {
			t.Run(string(provider)+"/"+scenario, func(t *testing.T) {
				api := newFakeAPI(t)
				c := testConfig(t, api)
				ctx := context.Background()
				const email, org = "work@example.test", "work-org"
				identity := func(address, organization string) string {
					return fakeJWT(map[string]any{"email": address, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": organization, "chatgpt_plan_type": "pro"}})
				}
				api.emails["old-access"] = email
				api.orgs["old-access"] = [2]string{org, "Work"}
				old := &Record{Provider: provider, AccessToken: "old-access", RefreshToken: "old-refresh", IDToken: identity(email, org), ExpiresAt: c.now().Add(-time.Hour).UnixMilli()}
				saved, err := c.persistAccount(ctx, old, "My work", "capture", false)
				if err != nil {
					t.Fatal(err)
				}
				key := saved.Record.StoreKey()
				if scenario == "missing credentials" {
					if err := c.tokenDelete(key); err != nil {
						t.Fatal(err)
					}
				}
				// A dead login has a recent cached error. New credentials must let
				// the next status fetch current usage without waiting five minutes.
				c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry {
					e.AttemptedAt = c.now().UnixMilli()
					e.LastError = "token refresh failed (401 @ provider): invalid_grant"
					return e
				})
				advance(time.Second)
				browserEmail, browserOrg := email, org
				if scenario == "wrong email" {
					browserEmail = "personal@example.test"
				}
				if scenario == "wrong organization" {
					browserOrg = "personal-org"
				}
				api.emails["new-access"] = browserEmail
				api.orgs["new-access"] = [2]string{browserOrg, "Work"}
				api.usage["new-access"] = claudeUsage
				if provider == Codex {
					api.usage["new-access"] = codexUsage
				}
				tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if provider == Codex {
						if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "authorization_code" {
							t.Errorf("invalid authorization grant: %v", err)
						}
					} else {
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["grant_type"] != "authorization_code" {
							t.Errorf("invalid authorization grant: %v", err)
						}
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600, "id_token": identity(browserEmail, browserOrg)}); err != nil {
						t.Error(err)
					}
				}))
				t.Cleanup(tokens.Close)
				c.ClaudeTokenURLs, c.CodexTokenURL = []string{tokens.URL}, tokens.URL
				// Only the selector chooses the provider, as in the menu bar action.
				session, err := c.BeginLogin(LoginOptions{Selector: key, Manual: provider == Claude})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(session.Cancel)
				if session.Provider != provider {
					t.Fatalf("selected provider = %s, want %s", session.Provider, provider)
				}
				if scenario == "removed during login" {
					if _, err := c.RemoveAccount(key, provider); err != nil {
						t.Fatal(err)
					}
				}
				result, err := c.CompleteLogin(ctx, session, "synthetic-code", "")
				idx, loadErr := c.LoadIndex()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if scenario == "removed during login" {
					if err == nil || len(idx.Accounts) != 0 {
						t.Fatalf("relink recreated a removed account: err=%v index=%+v", err, idx)
					}
					return
				}
				if len(idx.Accounts) != 1 || idx.Accounts[0].Label != "My work" {
					t.Fatalf("relink changed account list or label: %+v", idx)
				}
				stored, loadErr := c.tokenGet(key)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if strings.HasPrefix(scenario, "wrong") {
					if err == nil || !strings.Contains(err.Error(), "different account or organization") || stored.AccessToken != "old-access" || stored.RefreshToken != "old-refresh" {
						t.Fatalf("wrong browser account changed credentials: err=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.IsNew || stored.AccessToken != "new-access" || stored.RefreshToken != "new-refresh" {
					t.Fatal("relink did not replace the existing account's credentials")
				}
				snapshot, err := c.Collect(ctx, CollectOptions{Providers: []Provider{provider}, NoSync: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Results) != 1 || snapshot.Results[0].NeedsLogin || len(snapshot.Results[0].Usage) == 0 || api.usageHit.Load() != 1 {
					t.Fatalf("relink did not restore fresh stats: %+v, requests=%d", snapshot.Results, api.usageHit.Load())
				}
			})
		}
	}
}
