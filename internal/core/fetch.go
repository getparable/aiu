package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------- refresh + hand-back

// ensureFresh refreshes r when its access token is (nearly) expired, stores the new
// pair, and hands it back to the CLI when the CLI still holds the refresh token just
// spent — otherwise the CLI's next run would redeem a retired token and lose its login.
func (c *Config) ensureFresh(ctx context.Context, r *Record, liveClaude *LiveClaude, liveCodex *LiveCodex, force bool) (*Record, error) {
	if !force && !r.IsExpired(c.now(), refreshMargin) {
		return r, nil
	}
	if r.RefreshToken == "" {
		return nil, errors.New("no refresh token stored; sign in again for this account")
	}
	spent := r.RefreshToken
	next := *r
	if r.Provider == Codex {
		fresh, err := c.refreshCodex(ctx, r)
		if err != nil {
			return nil, err
		}
		next.AccessToken, next.RefreshToken, next.IDToken = fresh.AccessToken, fresh.RefreshToken, fresh.IDToken
		next.ExpiresAt, next.LastRefresh = fresh.ExpiresAt, fresh.LastRefresh
		next.AccountID = firstNonEmpty(fresh.AccountID, r.AccountID)
		next.PlanType = firstNonEmpty(fresh.PlanType, r.PlanType)
	} else {
		fresh, err := c.refreshClaude(ctx, spent)
		if err != nil {
			return nil, err
		}
		next.AccessToken, next.RefreshToken, next.ExpiresAt = fresh.AccessToken, fresh.RefreshToken, fresh.ExpiresAt
		if fresh.RefreshTokenExpiresAt > 0 {
			next.RefreshTokenExpiresAt = fresh.RefreshTokenExpiresAt
		}
		if fresh.Scopes != nil {
			next.Scopes = fresh.Scopes
		}
	}
	next.UpdatedAt = c.now().UnixMilli()
	if err := c.tokenSet(next.StoreKey(), &next); err != nil {
		return nil, fmt.Errorf("refreshed %s but could not store the new token: %w", r.Email, err)
	}
	switch {
	case r.Provider == Claude && liveClaude != nil && liveClaude.refreshToken() == spent:
		if written, err := c.writeClaudeCode(liveClaude, claudeTokenPatch(&next), false); err != nil {
			c.Warn(fmt.Sprintf("refreshed %s but could not update Claude Code's credentials: %v", r.Email, err))
		} else {
			*liveClaude = *written
		}
	case r.Provider == Codex && liveCodex != nil && liveCodex.refreshToken() == spent:
		if written, err := c.writeCodexAuth(liveCodex, &next); err != nil {
			c.Warn(fmt.Sprintf("refreshed %s but could not update Codex's auth.json: %v", r.Email, err))
		} else {
			*liveCodex = *written
		}
	}
	return &next, nil
}

// adoptClaudeLive takes Claude Code's token when it is newer than ours. Claude Code
// refreshes on its own schedule, which retires the refresh token we hold.
func (c *Config) adoptClaudeLive(r *Record, live *LiveClaude) (*Record, error) {
	if live.accessToken() == r.AccessToken || live.int64Field("expiresAt") <= r.ExpiresAt {
		return r, nil
	}
	next := *r
	next.AccessToken = live.accessToken()
	next.RefreshToken = firstNonEmpty(live.refreshToken(), r.RefreshToken)
	next.ExpiresAt = live.int64Field("expiresAt")
	if v := live.int64Field("refreshTokenExpiresAt"); v > 0 {
		next.RefreshTokenExpiresAt = v
	}
	if scopes, ok := live.OAuth["scopes"].([]any); ok {
		next.Scopes = next.Scopes[:0:0]
		for _, s := range scopes {
			next.Scopes = append(next.Scopes, str(s))
		}
	}
	next.SubscriptionType = firstNonEmpty(str(live.OAuth["subscriptionType"]), r.SubscriptionType)
	next.RateLimitTier = firstNonEmpty(str(live.OAuth["rateLimitTier"]), r.RateLimitTier)
	next.UpdatedAt = c.now().UnixMilli()
	return &next, c.tokenSet(next.StoreKey(), &next)
}

func (c *Config) adoptCodexLive(r *Record, live *LiveCodex) (*Record, error) {
	working := codexRecordFromLive(live)
	// Codex stamps last_refresh on every rotation, so newer there means newer.
	if working.AccessToken == r.AccessToken || working.LastRefresh <= r.LastRefresh {
		return r, nil
	}
	next := *r
	next.AccessToken, next.RefreshToken, next.IDToken = working.AccessToken, working.RefreshToken, working.IDToken
	next.ExpiresAt, next.LastRefresh = working.ExpiresAt, working.LastRefresh
	next.AccountID = firstNonEmpty(working.AccountID, r.AccountID)
	next.PlanType = firstNonEmpty(working.PlanType, r.PlanType)
	next.UpdatedAt = c.now().UnixMilli()
	return &next, c.tokenSet(next.StoreKey(), &next)
}

// LiveMatch is who a CLI is signed in as. Verified means a token matched or the
// profile endpoint confirmed it; otherwise Email is .claude.json's hint. Org is the
// organization (Claude) or ChatGPT account the login is scoped to: one address can
// hold several, each with its own limits.
type LiveMatch struct {
	Email    string
	Org      string
	Verified bool
}

// Is reports whether a record is the account this match names.
func (m LiveMatch) Is(r *Record) bool {
	if m.Email == "" || m.Email != r.Email {
		return false
	}
	// An unknown org matches any record for the address; we know no better.
	return m.Org == "" || r.OrgUUID == "" || m.Org == r.OrgUUID
}

func matchClaudeLive(c *Config, live *LiveClaude, records []*Record) LiveMatch {
	for _, r := range records {
		if r.Provider != Claude || r.Missing {
			continue
		}
		// Only a token present on both sides is evidence.
		if (live.refreshToken() != "" && r.RefreshToken == live.refreshToken()) ||
			(live.accessToken() != "" && r.AccessToken == live.accessToken()) {
			return LiveMatch{Email: r.Email, Org: r.OrgUUID, Verified: true}
		}
	}
	email, org := c.claudeCachedAccountIdentity()
	return LiveMatch{Email: email, Org: org}
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// resolveClaudeLive identifies Claude Code's token, asking the profile endpoint when
// no stored token matches. The answer is cached per token, and a failing lookup is
// not retried for a while: repeated knocking is what keeps a throttle from clearing.
func (c *Config) resolveClaudeLive(ctx context.Context, live *LiveClaude, records []*Record) LiveMatch {
	m := matchClaudeLive(c, live, records)
	if m.Verified || live.int64Field("expiresAt") <= c.now().UnixMilli() {
		return m
	}
	fp := fingerprint(live.accessToken())
	recent := c.readCache()[profileKey]
	// An entry without an organization cannot tell two organizations on one address
	// apart, so it is re-fetched rather than trusted.
	if recent != nil && recent.Fingerprint == fp && (recent.Email == "" || recent.Org != "") {
		if recent.Email != "" {
			return LiveMatch{Email: recent.Email, Org: recent.Org, Verified: true}
		}
		if c.now().UnixMilli()-recent.FailedAt < profileRetrySpacing.Milliseconds() {
			return m
		}
	}
	email, profile, err := c.fetchClaudeProfile(ctx, live.accessToken())
	now := c.now().UnixMilli()
	if err != nil {
		c.cacheUpdate(profileKey, func(*cacheEntry) *cacheEntry { return &cacheEntry{Fingerprint: fp, FailedAt: now} })
		c.Warn("could not verify the active Claude Code account: " + err.Error())
		return m
	}
	org := str(profile["organizationUuid"])
	c.cacheUpdate(profileKey, func(*cacheEntry) *cacheEntry {
		return &cacheEntry{Fingerprint: fp, Email: email, Org: org, VerifiedAt: now}
	})
	return LiveMatch{Email: email, Org: org, Verified: true}
}

// SyncClaude adopts Claude Code's newer token into the matching record.
func (c *Config) SyncClaude(ctx context.Context, records []*Record, verify bool) (*LiveClaude, LiveMatch, []*Record) {
	live := c.ReadClaudeCode()
	if live == nil {
		return nil, LiveMatch{}, records
	}
	var m LiveMatch
	if verify {
		m = c.resolveClaudeLive(ctx, live, records)
	} else {
		m = matchClaudeLive(c, live, records)
	}
	if !verify || !m.Verified {
		return live, m, records
	}
	out := make([]*Record, len(records))
	for i, r := range records {
		out[i] = r
		if r.Provider != Claude || r.Missing || !m.Is(r) {
			continue
		}
		next, err := c.adoptClaudeLive(r, live)
		if err != nil {
			c.Warn(fmt.Sprintf("could not store Claude Code's token for %s: %v", r.Email, err))
			continue
		}
		if next != r {
			c.Info("synced " + r.Email + " from Claude Code")
		}
		out[i] = next
	}
	return live, m, out
}

// SyncCodex is the Codex side; identity comes from auth.json, so it is purely local.
func (c *Config) SyncCodex(records []*Record, adopt bool) (*LiveCodex, LiveMatch, []*Record) {
	live := c.ReadCodexAuth()
	if live == nil {
		return nil, LiveMatch{}, records
	}
	id := live.identity()
	m := LiveMatch{Email: id.Email, Org: id.AccountID, Verified: id.Email != ""}
	out := make([]*Record, len(records))
	for i, r := range records {
		out[i] = r
		if !adopt || r.Provider != Codex || r.Missing || !m.Is(r) {
			continue
		}
		next, err := c.adoptCodexLive(r, live)
		if err != nil {
			c.Warn(fmt.Sprintf("could not store Codex's token for %s: %v", r.Email, err))
			continue
		}
		if next != r {
			c.Info("synced " + r.Email + " from Codex")
		}
		out[i] = next
	}
	return live, m, out
}

// ---------------------------------------------------------------- usage

var deadLogin = regexp.MustCompile(`(?i)invalid_grant|no refresh token|refresh token`)

// isDeadLoginError is an error meaning the stored login is gone, not that a request failed.
func isDeadLoginError(msg string) bool { return msg != "" && deadLogin.MatchString(msg) }

const throttleNote = "request throttling by the usage endpoint, not your subscription quota"

type usageResult struct {
	record    *Record
	usage     json.RawMessage
	fetchedAt int64
	stale     string
}

func (c *Config) rateLimitedMessage(until int64) string {
	return "rate limited — next try in " + FormatRelative(time.Duration(until-c.now().UnixMilli())*time.Millisecond)
}

// fetchUsage returns one account's usage, going to the network only when the
// machine-wide timing policy allows; otherwise it answers from the cache.
func (c *Config) fetchUsage(ctx context.Context, r *Record, liveClaude *LiveClaude, liveCodex *LiveCodex) (*usageResult, error) {
	key := r.StoreKey()
	var g gate
	for {
		var err error
		g, err = c.claimFetchSlot(key, r.UpdatedAt)
		if errors.Is(err, errLocked) {
			// Cannot coordinate, so do the safe thing: answer from the cache.
			e := cacheEntry{}
			if cached := c.readCache()[key]; cached != nil {
				e = *cached
			}
			g = gate{kind: gateSpacing, entry: e}
		} else if err != nil {
			return nil, err
		}
		if g.kind == gateWait {
			sleep(ctx, g.wait)
			continue
		}
		if g.kind == gateInflight {
			sleep(ctx, 500*time.Millisecond)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		break
	}
	e := g.entry
	switch g.kind {
	case gateCooldown:
		if e.Usage != nil {
			return &usageResult{record: r, usage: e.Usage, fetchedAt: e.FetchedAt, stale: c.rateLimitedMessage(e.LimitedUntil)}, nil
		}
		return nil, fmt.Errorf("%s (%s)", c.rateLimitedMessage(e.LimitedUntil), throttleNote)
	case gateSpacing:
		// A dead login stays dead until someone signs in again; answering from the
		// cache would show the account healthy everywhere but the process that failed.
		if e.Usage != nil && !isDeadLoginError(e.LastError) {
			stale := ""
			if e.LastError != "" {
				stale = e.LastError + " — showing values from " + FormatRelative(time.Duration(c.now().UnixMilli()-e.FetchedAt)*time.Millisecond) + " ago"
			}
			return &usageResult{record: r, usage: e.Usage, fetchedAt: e.FetchedAt, stale: stale}, nil
		}
		if e.LastError != "" {
			return nil, errors.New(e.LastError)
		}
		return nil, errors.New("no usage data yet — another process is fetching it, try again in a moment")
	}

	current, err := c.ensureFresh(ctx, r, liveClaude, liveCodex, false)
	var res *apiResponse
	if err == nil {
		res, err = c.usageRequest(ctx, current)
		if err == nil && res.Status == 401 {
			current, err = c.ensureFresh(ctx, current, liveClaude, liveCodex, true)
			if err == nil {
				res, err = c.usageRequest(ctx, current)
			}
		}
	}
	if err != nil {
		msg := Redact(err.Error())
		c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry { e.LastError = msg; return e })
		return nil, err
	}
	now := c.now().UnixMilli()
	switch {
	case res.Status == 429:
		strikes := e.Strikes + 1
		cooldown := rateLimitCooldown << (strikes - 1)
		if cooldown > rateLimitCooldownMx || cooldown <= 0 {
			cooldown = rateLimitCooldownMx
		}
		if secs, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && time.Duration(secs)*time.Second > cooldown {
			cooldown = time.Duration(secs) * time.Second
		}
		until := now + cooldown.Milliseconds()
		c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry {
			e.LimitedUntil, e.Strikes, e.LastLimitedAt, e.LastError = until, strikes, now, "rate limited"
			return e
		})
		if e.Usage != nil {
			return &usageResult{record: current, usage: e.Usage, fetchedAt: e.FetchedAt, stale: c.rateLimitedMessage(until)}, nil
		}
		return nil, fmt.Errorf("%s (%s)", c.rateLimitedMessage(until), throttleNote)
	case res.Status >= 500:
		vendor := "Anthropic"
		if r.Provider == Codex {
			vendor = "OpenAI"
		}
		msg := fmt.Sprintf("%s returned %d", vendor, res.Status)
		c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry { e.LastError = msg; return e })
		if e.Usage != nil {
			return &usageResult{record: current, usage: e.Usage, fetchedAt: e.FetchedAt, stale: msg + " — showing the last known values"}, nil
		}
		return nil, errors.New(msg + " — temporary, the next refresh should recover")
	case !res.ok():
		msg := fmt.Sprintf("usage %d: %s", res.Status, res.describe())
		c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry { e.LastError = msg; return e })
		return nil, errors.New(msg)
	}
	if !json.Valid(res.Body) {
		return nil, errors.New("usage endpoint returned a non-JSON body")
	}
	// A full replace: drops strikes, the cooldown and old errors; only the memory of
	// a recent 429 carries over.
	c.cacheUpdate(key, func(old *cacheEntry) *cacheEntry {
		return &cacheEntry{Usage: res.Body, FetchedAt: now, AttemptedAt: g.claimedAt, LastLimitedAt: old.LastLimitedAt}
	})
	return &usageResult{record: current, usage: res.Body, fetchedAt: now}, nil
}

func (c *Config) usageRequest(ctx context.Context, r *Record) (*apiResponse, error) {
	if r.Provider == Codex {
		return c.apiGet(ctx, c.CodexUsageURL, r.AccessToken, map[string]string{"ChatGPT-Account-Id": r.AccountID})
	}
	return c.apiGet(ctx, c.ClaudeUsageURL, r.AccessToken, map[string]string{"anthropic-beta": claudeOAuthBeta})
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------- collect

// Result is one tracked account with its usage, or why there is none.
type Result struct {
	Record     *Record
	Active     bool
	Usage      json.RawMessage
	FetchedAt  int64
	Stale      string
	Err        string
	NeedsLogin bool
}

// Snapshot is every tracked account plus who each CLI is signed in as.
type Snapshot struct {
	Results []*Result
	Live    map[Provider]LiveMatch
	Empty   bool
}

// CollectOptions narrows and tunes a collection.
type CollectOptions struct {
	Providers []Provider // empty means all
	NoSync    bool       // do not adopt tokens from the CLIs
}

// Collect gathers usage for every tracked account. Requests that do go out are
// still spaced machine-wide, but their round trips overlap.
func (c *Config) Collect(ctx context.Context, opts CollectOptions) (*Snapshot, error) {
	idx, err := c.LoadIndex()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Live: map[Provider]LiveMatch{}}
	if len(idx.Accounts) == 0 {
		snap.Empty = true
		return snap, nil
	}
	records, err := c.LoadRecords(idx)
	if err != nil {
		return nil, err
	}
	if len(opts.Providers) > 0 {
		// Filter before fetching, so a one-provider view never spends the other's budget.
		kept := records[:0]
		for _, r := range records {
			for _, p := range opts.Providers {
				if r.Provider == p {
					kept = append(kept, r)
				}
			}
		}
		records = kept
	}
	liveClaude, claudeMatch, records := c.SyncClaude(ctx, records, !opts.NoSync)
	liveCodex, codexMatch, records := c.SyncCodex(records, !opts.NoSync)
	snap.Live[Claude], snap.Live[Codex] = claudeMatch, codexMatch

	// The live docs are shared by every goroutine that may hand a refresh back.
	var mu sync.Mutex
	results := make([]*Result, len(records))
	var wg sync.WaitGroup
	for i, r := range records {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := &Result{Record: r, Active: snap.Live[r.Provider].Is(r)}
			results[i] = res
			if r.Missing {
				res.Err, res.NeedsLogin = "token not found in store — sign in again for this account", true
				return
			}
			var lc *LiveClaude
			var lx *LiveCodex
			if r.Provider == Claude {
				lc = liveClaude
			} else {
				lx = liveCodex
			}
			u, err := c.fetchUsageLocked(ctx, &mu, r, lc, lx)
			if err != nil {
				res.Err, res.NeedsLogin = Redact(err.Error()), isDeadLoginError(err.Error())
				return
			}
			res.Record, res.Usage, res.FetchedAt, res.Stale = u.record, u.usage, u.fetchedAt, u.stale
		}()
	}
	wg.Wait()
	snap.Results = results
	return snap, nil
}

// fetchUsageLocked serialises only the refresh hand-back to a shared live doc; two
// accounts never share one, so the lock is uncontended in practice.
func (c *Config) fetchUsageLocked(ctx context.Context, mu *sync.Mutex, r *Record, lc *LiveClaude, lx *LiveCodex) (*usageResult, error) {
	if (lc != nil && r.RefreshToken == lc.refreshToken()) || (lx != nil && r.RefreshToken == lx.refreshToken()) {
		mu.Lock()
		defer mu.Unlock()
	}
	return c.fetchUsage(ctx, r, lc, lx)
}
