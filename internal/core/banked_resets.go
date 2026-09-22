package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BankedResets describes the credit count and cached credit details for a Codex account.
type BankedResets struct {
	AvailableCount            *int                `json:"availableCount"`
	Credits                   []BankedResetCredit `json:"credits"`
	FetchedAt                 string              `json:"fetchedAt,omitempty"`
	Stale                     string              `json:"stale,omitempty"`
	Error                     string              `json:"error,omitempty"`
	CanRedeem                 bool                `json:"canRedeem"`
	PendingRequest            *BankedResetRequest `json:"pendingRequest,omitempty"`
	AutoReset                 bool                `json:"autoReset"`
	AutoResetThresholdPercent int                 `json:"autoResetThresholdPercent"`
	AutoResetStatus           string              `json:"autoResetStatus,omitempty"`
}

type BankedResetCredit struct {
	ID          string `json:"id"`
	ResetType   string `json:"resetType"`
	Status      string `json:"status"`
	GrantedAt   string `json:"grantedAt"`
	ExpiresAt   string `json:"expiresAt"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CanRedeem   bool   `json:"canRedeem"`
}

type BankedResetRequest struct {
	RequestID string `json:"requestId"`
	CreditID  string `json:"creditId,omitempty"`
}

type BankedResetRedemption struct {
	Code         string `json:"code"`
	WindowsReset int    `json:"windowsReset"`
	RequestID    string `json:"requestId"`
	Message      string `json:"message,omitempty"`
}

type resetCreditWire struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"reset_type"`
	Status      string  `json:"status"`
	GrantedAt   string  `json:"granted_at"`
	ExpiresAt   *string `json:"expires_at"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}
type resetListWire struct {
	AvailableCount   *int              `json:"available_count"`
	Credits          []resetCreditWire `json:"credits"`
	TotalEarnedCount int               `json:"total_earned_count"`
}
type resetPostWire struct {
	Code         string `json:"code"`
	WindowsReset *int   `json:"windows_reset"`
}
type resetState struct {
	Accounts map[string]*resetAccountState `json:"accounts"`
}
type resetAccountState struct {
	Details                     *BankedResets             `json:"details,omitempty"`
	Pending                     *BankedResetRequest       `json:"pending,omitempty"`
	Completed                   map[string]completedReset `json:"completed,omitempty"`
	AutoEnabled                 bool                      `json:"autoEnabled,omitempty"`
	AutoArmed                   bool                      `json:"autoArmed,omitempty"`
	AutoStatus                  string                    `json:"autoStatus,omitempty"`
	AutoRequestID               string                    `json:"autoRequestId,omitempty"`
	AutoCreditID                string                    `json:"autoCreditId,omitempty"`
	AutoResetThresholdPercent   *int                      `json:"autoResetThresholdPercent,omitempty"`
	LastResetThresholdPercent   *int                      `json:"lastResetThresholdPercent,omitempty"`
	PendingThresholdPercent     *int                      `json:"pendingThresholdPercent,omitempty"`
	AutoRequestThresholdPercent *int                      `json:"autoRequestThresholdPercent,omitempty"`
	LastResetAttemptAt          int64                     `json:"lastResetAttemptAt,omitempty"`
	DetailsAttemptedAt          int64                     `json:"detailsAttemptedAt,omitempty"`
}
type completedReset struct {
	CreditID         string                `json:"creditId"`
	Result           BankedResetRedemption `json:"result"`
	ThresholdPercent *int                  `json:"thresholdPercent,omitempty"`
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (c *Config) resetStatePath() string { return filepath.Join(c.Dir, "banked-resets.json") }
func (c *Config) resetLockPath() string  { return filepath.Join(c.Dir, "banked-resets.lock") }
func (c *Config) resetAccountLockPath(key string) string {
	return c.resetLockPath() + "." + fingerprint(key)
}

func (c *Config) loadResetState() (resetState, error) {
	s := resetState{Accounts: map[string]*resetAccountState{}}
	_, err := readJSONFile(c.resetStatePath(), &s)
	if s.Accounts == nil {
		s.Accounts = map[string]*resetAccountState{}
	}
	return s, err
}

func (c *Config) withResetState(fn func(*resetState) error) error {
	return withFileLock(c.resetLockPath(), func() error {
		s, err := c.loadResetState()
		if err != nil {
			return err
		}
		if err := fn(&s); err != nil {
			return err
		}
		return writePrivateJSON(c.resetStatePath(), s)
	})
}

func resetAccount(s *resetState, key string) *resetAccountState {
	a := s.Accounts[key]
	if a == nil {
		a = &resetAccountState{Completed: map[string]completedReset{}}
		s.Accounts[key] = a
	}
	if a.Completed == nil {
		a.Completed = map[string]completedReset{}
	}
	return a
}

func effectiveAutoResetThreshold(a *resetAccountState) int {
	if a == nil || a.AutoResetThresholdPercent == nil {
		return 1
	}
	return *a.AutoResetThresholdPercent
}

func countFromUsage(raw json.RawMessage) *int {
	var v struct {
		Count *struct {
			Available *int `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Count == nil || v.Count.Available == nil || *v.Count.Available < 0 {
		return nil
	}
	n := *v.Count.Available
	return &n
}

func (c *Config) cachedBankedResets(r *Record, usage json.RawMessage, stale string) *BankedResets {
	out := &BankedResets{AvailableCount: nil, Credits: nil, Stale: stale, AutoResetThresholdPercent: 1}
	var detailAt int64
	err := withFileLock(c.resetLockPath(), func() error {
		s, err := c.loadResetState()
		if err != nil {
			return err
		}
		if a := s.Accounts[r.StoreKey()]; a != nil && a.Details != nil {
			d := *a.Details
			if d.Credits != nil {
				out.Credits = append([]BankedResetCredit{}, d.Credits...)
			}
			for i := range out.Credits {
				out.Credits[i].CanRedeem = resetCreditUsable(out.Credits[i], c.now())
			}
			out.FetchedAt = d.FetchedAt
			if t, e := time.Parse(time.RFC3339Nano, d.FetchedAt); e == nil {
				detailAt = t.UnixMilli()
			}
			out.Error = d.Error
			out.AvailableCount = d.AvailableCount
		}
		if a := s.Accounts[r.StoreKey()]; a != nil {
			out.PendingRequest = a.Pending
			out.AutoReset, out.AutoResetStatus = a.AutoEnabled, a.AutoStatus
			out.AutoResetThresholdPercent = effectiveAutoResetThreshold(a)
			if out.AutoResetThresholdPercent < 0 || out.AutoResetThresholdPercent > 99 {
				out.Error = "invalid stored auto-reset threshold; update settings before automatic redemption"
				out.AutoResetStatus = out.Error
				out.AutoResetThresholdPercent = 1
			}
		}
		return nil
	})
	if err != nil {
		out.Error = Redact(err.Error())
	}
	if usageCount := countFromUsage(usage); usageCount != nil {
		cached := c.readCache()[r.StoreKey()]
		if cached == nil || cached.FetchedAt >= detailAt {
			out.AvailableCount = usageCount
		}
	}
	if out.AvailableCount == nil {
		out.AvailableCount = countFromUsage(usage)
	}
	out.CanRedeem = err == nil && !r.Missing && out.PendingRequest == nil && out.AvailableCount != nil && *out.AvailableCount > 0
	if cached := c.readCache()[r.StoreKey()]; cached != nil && cached.LimitedUntil > c.now().UnixMilli() {
		out.CanRedeem = false
	}
	return out
}

// SetAutoReset preserves the configured threshold while changing the enabled flag.
func (c *Config) SetAutoReset(ctx context.Context, target string, provider Provider, enabled bool) error {
	return c.ConfigureAutoReset(ctx, target, provider, &enabled, nil)
}

// ConfigureAutoReset applies only the supplied per-account settings atomically.
func (c *Config) ConfigureAutoReset(ctx context.Context, target string, provider Provider, enabled *bool, thresholdPercent *int) error {
	if enabled == nil && thresholdPercent == nil {
		return errors.New("at least one auto-reset setting is required")
	}
	if thresholdPercent != nil && (*thresholdPercent < 0 || *thresholdPercent > 99) {
		return errors.New("auto-reset threshold must be between 0 and 99 percent remaining")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if provider != "" && provider != Codex {
		return errors.New("banked resets are available only for Codex accounts")
	}
	idx, e, err := c.FindAccount(target, Codex)
	if err != nil {
		return err
	}
	if e.Provider != Codex {
		return errors.New("banked resets are available only for Codex accounts")
	}
	key := storeKey(e.Provider, e.Email, e.Org)
	if idx == nil {
		return errors.New("no tracked Codex account matches")
	}
	return withFileLock(c.resetAccountLockPath(key), func() error {
		return c.withResetState(func(s *resetState) error {
			a := resetAccount(s, key)
			if thresholdPercent != nil {
				v := *thresholdPercent
				a.AutoResetThresholdPercent = &v
			}
			if enabled != nil && *enabled {
				a.AutoEnabled = true
				if a.AutoRequestID == "" && a.Pending == nil {
					a.AutoArmed = true
					a.AutoStatus = "waiting for fresh usage"
				}
			} else if enabled != nil {
				a.AutoEnabled = false
				a.AutoArmed = false
				a.AutoStatus = "disabled"
			}
			return nil
		})
	})
}

func usageThreshold(raw json.RawMessage, triggerPercent, rearmPercent int) (high, triggerKnown, allRecovered bool) {
	var v struct {
		RateLimit json.RawMessage `json:"rate_limit"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.RateLimit) == 0 {
		return false, false, false
	}
	var limits map[string]json.RawMessage
	if json.Unmarshal(v.RateLimit, &limits) != nil {
		return false, false, false
	}
	known, recovered := false, true
	for _, key := range []string{"primary_window", "secondary_window"} {
		b, exists := limits[key]
		if !exists || string(b) == "null" {
			continue
		}
		var window struct {
			Used *float64 `json:"used_percent"`
		}
		if json.Unmarshal(b, &window) != nil || window.Used == nil {
			recovered = false
			continue
		}
		n := *window.Used
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
			recovered = false
			continue
		}
		known = true
		if n >= float64(100-triggerPercent) {
			high = true
		}
		if n >= float64(100-rearmPercent) {
			recovered = false
		}
	}
	return high, known, known && recovered
}

func usageResetThreshold(raw json.RawMessage) (high, known, belowDefault bool) {
	return usageThreshold(raw, 1, 1)
}

// maybeAutoReset runs only after a fresh successful usage response. Its per-account
// lock serializes preference changes, concurrent Collect calls, and redemption.
func (c *Config) maybeAutoReset(ctx context.Context, r *Record, usage json.RawMessage, usageStartedAt int64) {
	count := countFromUsage(usage)
	_ = withFileLock(c.resetAccountLockPath(r.StoreKey()), func() error {
		var req *BankedResetRequest
		var shouldRedeem bool
		err := c.withResetState(func(s *resetState) error {
			a := s.Accounts[r.StoreKey()]
			if a == nil || !a.AutoEnabled {
				return nil
			}
			threshold := effectiveAutoResetThreshold(a)
			if threshold < 0 || threshold > 99 {
				a.AutoStatus = "invalid stored auto-reset threshold; update settings before automatic redemption"
				return nil
			}
			attemptThreshold := 1
			if a.LastResetThresholdPercent != nil {
				attemptThreshold = *a.LastResetThresholdPercent
			}
			high, known, _ := usageThreshold(usage, threshold, threshold)
			_, _, recoveredBoth := usageThreshold(usage, threshold, max(threshold, attemptThreshold))
			if a.AutoRequestID != "" {
				if done, ok := a.Completed[a.AutoRequestID]; ok && done.CreditID == a.AutoCreditID {
					rearmThreshold := attemptThreshold
					if done.ThresholdPercent != nil {
						rearmThreshold = *done.ThresholdPercent
					}
					_, _, recoveredBoth = usageThreshold(usage, threshold, max(threshold, rearmThreshold))
					if recoveredBoth && usageStartedAt > a.LastResetAttemptAt {
						a.AutoRequestID = ""
						a.AutoCreditID = ""
						a.AutoRequestThresholdPercent = nil
						a.AutoArmed = true
						a.AutoStatus = "armed"
					} else {
						a.AutoStatus = "reset result: " + done.Result.Code
					}
					return nil
				}
			}
			if a.Pending != nil {
				if a.AutoRequestID != "" && a.Pending.RequestID == a.AutoRequestID && a.Pending.CreditID == a.AutoCreditID {
					req = a.Pending
					shouldRedeem = true
					a.AutoStatus = "retrying unresolved automatic reset"
				} else {
					a.AutoStatus = "another reset request is unresolved"
				}
				return nil
			}
			// Resolve an uncertain request first, even when usage has recovered or the
			// latest count is zero. The idempotency key remains unchanged.
			if a.AutoRequestID != "" {
				req = &BankedResetRequest{RequestID: a.AutoRequestID, CreditID: a.AutoCreditID}
				shouldRedeem = true
				a.AutoStatus = "retrying unresolved automatic reset"
				return nil
			}
			if usageStartedAt <= a.LastResetAttemptAt {
				a.AutoStatus = "waiting for usage fetched after the last reset attempt"
				return nil
			}
			if !known {
				a.AutoStatus = "waiting for complete fresh usage"
				return nil
			}
			if !high {
				if recoveredBoth {
					a.AutoArmed = true
					a.AutoRequestID = ""
					a.AutoCreditID = ""
					a.AutoRequestThresholdPercent = nil
					a.AutoStatus = "armed"
				}
				return nil
			}
			if count == nil || *count <= 0 {
				a.AutoStatus = "no reset credits available"
				return nil
			}
			if a.Details == nil {
				a.Details = &BankedResets{}
			}
			countCopy := *count
			a.Details.AvailableCount = &countCopy
			if !a.AutoArmed {
				if a.AutoStatus == "" {
					a.AutoStatus = "waiting for usage to recover above the configured remaining threshold"
				}
				return nil
			}
			id, e := newUUID()
			if e != nil {
				a.AutoStatus = "could not create reset request"
				return nil
			}
			a.AutoArmed = false
			a.AutoRequestID = id
			a.AutoCreditID = ""
			v := threshold
			a.AutoRequestThresholdPercent = &v
			a.AutoStatus = "redeeming reset"
			req = &BankedResetRequest{RequestID: id}
			shouldRedeem = true
			return nil
		})
		if err != nil || !shouldRedeem || req == nil {
			return err
		}
		out, e := c.consumeBankedReset(ctx, r.Email+"#"+r.OrgUUID, Codex, req.CreditID, req.RequestID)
		_ = c.withResetState(func(s *resetState) error {
			a := resetAccount(s, r.StoreKey())
			if e != nil {
				a.AutoStatus = "automatic reset unresolved; retrying same request"
			} else if out != nil {
				a.AutoStatus = "reset result: " + out.Code
			}
			return nil
		})
		return nil
	})
}

func (c *Config) resolveCodexRecord(target string, provider Provider) (*Record, error) {
	if provider != "" && provider != Codex {
		return nil, errors.New("banked resets are available only for Codex accounts")
	}
	idx, e, err := c.FindAccount(target, Codex)
	if err != nil {
		return nil, err
	}
	if e.Provider != Codex {
		return nil, errors.New("banked resets are available only for Codex accounts")
	}
	rs, err := c.LoadRecords(idx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Email == e.Email && r.OrgUUID == e.Org && r.Provider == Codex {
			if r.Missing {
				return nil, errors.New("Codex token is missing; sign in again for this account")
			}
			return r, nil
		}
	}
	return nil, errors.New("Codex token is missing; sign in again for this account")
}

func (c *Config) codexResetRecord(ctx context.Context, target string, provider Provider) (*Record, error) {
	r, err := c.resolveCodexRecord(target, provider)
	if err != nil {
		return nil, err
	}
	// A newer external Codex login is adopted only when its full account identity matches.
	if live := c.ReadCodexAuth(); live != nil && live.identity().Email == r.Email && live.identity().AccountID == r.AccountID {
		if next, e := c.adoptCodexLive(r, live); e == nil {
			r = next
		}
	}
	if r.IsExpired(c.now(), refreshMargin) {
		if live := c.ReadCodexAuth(); live != nil && live.identity().Email == r.Email && live.identity().AccountID == r.AccountID {
			return nil, errors.New("active Codex login is expired; sign in with Codex before using reset credits")
		}
		return c.ensureFresh(ctx, r, nil, nil, false)
	}
	return r, nil
}

// ListBankedResets returns locally cached details immediately; network refreshes share the
// usage endpoint spacing/cooldown policy for this account.
func (c *Config) ListBankedResets(ctx context.Context, target string, provider Provider) (*BankedResets, error) {
	if err := c.validateConfiguredResetURL(); err != nil {
		return nil, err
	}
	r, err := c.codexResetRecord(ctx, target, provider)
	if err != nil {
		return nil, err
	}
	var out *BankedResets
	err = withFileLock(c.resetAccountLockPath(r.StoreKey()), func() error { var e error; out, e = c.listBankedResets(ctx, r); return e })
	return out, err
}

func (c *Config) listBankedResets(ctx context.Context, r *Record) (*BankedResets, error) {
	usage := c.readCache()[r.StoreKey()]
	var raw json.RawMessage
	var stale string
	if usage != nil {
		raw = usage.Usage
		if usage.LimitedUntil > c.now().UnixMilli() {
			stale = c.rateLimitedMessage(usage.LimitedUntil)
		}
	}
	out := c.cachedBankedResets(r, raw, stale)
	if usage != nil && usage.LimitedUntil > c.now().UnixMilli() {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	allowed, waitReason, e := c.claimResetDetailSlot(r.StoreKey())
	if e != nil {
		return out, e
	}
	if !allowed {
		out.Stale = waitReason
		return out, nil
	}
	res, e := c.resetRequest(ctx, http.MethodGet, c.CodexResetCreditsURL, map[string]string{"Authorization": "Bearer " + r.AccessToken, "ChatGPT-Account-Id": r.AccountID}, nil)
	if e != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.Error = Redact(e.Error())
		return out, c.persistResetListError(r.StoreKey(), out.Error)
	}
	if res.Status == 429 {
		c.recordReset429(r.StoreKey(), res)
		out.Error = "rate limited"
		return out, c.persistResetListError(r.StoreKey(), out.Error)
	}
	if !res.ok() {
		out.Error = fmt.Sprintf("reset credits %d: %s", res.Status, res.describe())
		return out, c.persistResetListError(r.StoreKey(), out.Error)
	}
	var wire resetListWire
	if err := json.Unmarshal(res.Body, &wire); err != nil || wire.AvailableCount == nil || *wire.AvailableCount < 0 || wire.Credits == nil {
		out.Error = "reset credits endpoint returned an invalid response"
		return out, c.persistResetListError(r.StoreKey(), out.Error)
	}
	credits := make([]BankedResetCredit, 0, len(wire.Credits))
	for _, x := range wire.Credits {
		credit := BankedResetCredit{ID: x.ID, ResetType: x.ResetType, Status: x.Status, GrantedAt: x.GrantedAt}
		if x.ExpiresAt != nil {
			credit.ExpiresAt = *x.ExpiresAt
		}
		if x.Title != nil {
			credit.Title = *x.Title
		}
		if x.Description != nil {
			credit.Description = *x.Description
		}
		credit.CanRedeem = resetCreditUsable(credit, c.now())
		credits = append(credits, credit)
	}
	d := &BankedResets{AvailableCount: wire.AvailableCount, Credits: credits, FetchedAt: c.now().UTC().Format(time.RFC3339), CanRedeem: *wire.AvailableCount > 0}
	err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, r.StoreKey())
		d.PendingRequest = a.Pending
		d.AutoReset, d.AutoResetStatus = a.AutoEnabled, a.AutoStatus
		d.AutoResetThresholdPercent = effectiveAutoResetThreshold(a)
		a.Details = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (c *Config) persistResetListError(key, msg string) error {
	return c.withResetState(func(s *resetState) error {
		a := resetAccount(s, key)
		if a.Details == nil {
			a.Details = &BankedResets{}
		}
		a.Details.Error = msg
		return nil
	})
}

func resetCreditUsable(x BankedResetCredit, now time.Time) bool {
	if x.ID == "" || x.ResetType != "codex_rate_limits" || x.Status != "available" {
		return false
	}
	if x.ExpiresAt != "" {
		t, e := time.Parse(time.RFC3339, x.ExpiresAt)
		if e != nil || !t.After(now) {
			return false
		}
	}
	return true
}

func (c *Config) recordReset429(key string, res *apiResponse) {
	seconds, e := strconv.ParseUint(strings.TrimSpace(res.Header.Get("Retry-After")), 10, 64)
	cooldown := rateLimitCooldown
	old := c.readCache()[key]
	strikes := 1
	if old != nil {
		strikes = old.Strikes + 1
	}
	if strikes > 1 {
		cooldown = rateLimitCooldown << min(strikes-1, 7)
	}
	if cooldown > rateLimitCooldownMx {
		cooldown = rateLimitCooldownMx
	}
	const maxRetry = 365 * 24 * time.Hour
	if (e == nil && seconds > 0) || errors.Is(e, strconv.ErrRange) {
		retry := maxRetry
		if e == nil && seconds <= uint64(maxRetry/time.Second) {
			retry = time.Duration(seconds) * time.Second
		}
		if retry > cooldown {
			cooldown = retry
		}
	} else if e != nil {
		if retry, parseErr := http.ParseTime(res.Header.Get("Retry-After")); parseErr == nil && retry.Sub(c.now()) > cooldown {
			cooldown = min(retry.Sub(c.now()), maxRetry)
		}
	}
	until := c.now().Add(cooldown).UnixMilli()
	c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry {
		e.LimitedUntil = max(e.LimitedUntil, until)
		e.Strikes = max(e.Strikes+1, strikes)
		e.LastLimitedAt = c.now().UnixMilli()
		e.LastError = "rate limited"
		return e
	})
}

func (c *Config) claimResetDetailSlot(key string) (bool, string, error) {
	var allowed bool
	var reason string
	err := c.withResetState(func(s *resetState) error {
		now := c.now().UnixMilli()
		cachedEntries := c.readCache()
		if e := cachedEntries[key]; e != nil && e.LimitedUntil > now {
			reason = c.rateLimitedMessage(e.LimitedUntil)
			return nil
		}
		a := resetAccount(s, key)
		since := now - a.DetailsAttemptedAt
		spacing := MinFetchSpacing.Milliseconds()
		if a.DetailsAttemptedAt > 0 && since < spacing-spacingTolerance.Milliseconds() {
			reason = "reset details were fetched recently; try again in " + FormatRelative(time.Duration(spacing-since)*time.Millisecond)
			return nil
		}
		wall := time.Now().UnixMilli()
		var tooSoon bool
		err := c.withCacheLock(func(data cache) bool {
			machine := data[machineKey]
			if machine != nil && wall-machine.AttemptedAt >= 0 && wall-machine.AttemptedAt < accountStagger.Milliseconds() {
				tooSoon = true
				return false
			}
			data[machineKey] = &cacheEntry{AttemptedAt: wall}
			return true
		})
		if err != nil {
			return err
		}
		if tooSoon {
			reason = "another account was just refreshed; try reset details again in a moment"
			return nil
		}
		a.DetailsAttemptedAt = now
		allowed = true
		return nil
	})
	return allowed, reason, err
}

func (c *Config) ConsumeBankedReset(ctx context.Context, target string, provider Provider, creditID, requestID string) (*BankedResetRedemption, error) {
	if err := c.validateConfiguredResetURL(); err != nil {
		return nil, err
	}
	r, err := c.resolveCodexRecord(target, provider)
	if err != nil {
		return nil, err
	}
	var out *BankedResetRedemption
	err = withFileLock(c.resetAccountLockPath(r.StoreKey()), func() error {
		var e error
		out, e = c.consumeBankedReset(ctx, target, provider, creditID, requestID)
		return e
	})
	return out, err
}

func (c *Config) consumeBankedReset(ctx context.Context, target string, provider Provider, creditID, requestID string) (*BankedResetRedemption, error) {
	r, err := c.resolveCodexRecord(target, provider)
	if err != nil {
		return nil, err
	}
	if requestID == "" {
		requestID, err = newUUID()
		if err != nil {
			return nil, err
		}
	} else if !uuidPattern.MatchString(requestID) {
		return nil, errors.New("request ID must be a UUID")
	}
	key := r.StoreKey()
	var cached *BankedResetRedemption
	newPending := false
	var prior *BankedResetRedemption
	err = withFileLock(c.resetLockPath(), func() error {
		s, e := c.loadResetState()
		if e != nil {
			return e
		}
		if a := s.Accounts[key]; a != nil {
			if done, ok := a.Completed[requestID]; ok {
				if done.CreditID != creditID {
					return errors.New("request ID was already used for a different credit ID")
				}
				copy := done.Result
				prior = &copy
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if prior != nil {
		return prior, nil
	}
	if e := c.readCache()[key]; e != nil && e.LimitedUntil > c.now().UnixMilli() {
		return nil, fmt.Errorf("%s (%s)", c.rateLimitedMessage(e.LimitedUntil), throttleNote)
	}
	r, err = c.codexResetRecord(ctx, target, provider)
	if err != nil {
		return nil, err
	}
	err = c.withResetState(func(s *resetState) error {
		a := resetAccount(s, key)
		if cached := c.readCache()[key]; cached != nil {
			if count := countFromUsage(cached.Usage); count != nil {
				detailAt := int64(0)
				if a.Details != nil {
					if t, e := time.Parse(time.RFC3339Nano, a.Details.FetchedAt); e == nil {
						detailAt = t.UnixMilli()
					}
				}
				if a.Details == nil || a.Details.AvailableCount == nil || cached.FetchedAt >= detailAt {
					if a.Details == nil {
						a.Details = &BankedResets{}
					}
					a.Details.AvailableCount = count
				}
			}
		}
		if done, ok := a.Completed[requestID]; ok {
			if done.CreditID != creditID {
				return errors.New("request ID was already used for a different credit ID")
			}
			if done.Result.Code != "" && done.Result.RequestID == requestID {
				copy := done.Result
				cached = &copy
				return nil
			}
		}
		if a.Pending != nil {
			if a.Pending.RequestID != requestID || a.Pending.CreditID != creditID {
				return errors.New("another reset redemption is unresolved; retry it with the same request ID and credit ID")
			}
			return nil
		}
		if a.Details == nil || a.Details.AvailableCount == nil || *a.Details.AvailableCount <= 0 {
			return errors.New("reset credit details are not loaded; refresh the account first")
		}
		if creditID != "" {
			valid := false
			for _, x := range a.Details.Credits {
				if x.ID == creditID && resetCreditUsable(x, c.now()) {
					valid = true
				}
			}
			if !valid {
				return errors.New("credit is unavailable, expired, or unsupported")
			}
		}
		a.Pending = &BankedResetRequest{RequestID: requestID, CreditID: creditID}
		threshold := effectiveAutoResetThreshold(a)
		if a.AutoRequestID == requestID {
			threshold = 1 // legacy auto intent without a captured threshold
			if a.AutoRequestThresholdPercent != nil {
				threshold = *a.AutoRequestThresholdPercent
			}
		}
		a.PendingThresholdPercent = &threshold
		newPending = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cached != nil {
		return cached, nil
	}
	if err = ctx.Err(); err != nil {
		if newPending {
			_ = c.withResetState(func(s *resetState) error {
				if a := s.Accounts[key]; a != nil && a.Pending != nil && a.Pending.RequestID == requestID && a.Pending.CreditID == creditID {
					a.Pending = nil
					a.PendingThresholdPercent = nil
				}
				return nil
			})
		}
		return nil, fmt.Errorf("%w before reset request was sent; retry with request ID %s", err, requestID)
	}
	dispatchAt := c.now().UnixMilli()
	if err = c.withResetState(func(s *resetState) error {
		a := s.Accounts[key]
		if a == nil || a.Pending == nil || a.Pending.RequestID != requestID || a.Pending.CreditID != creditID {
			return errors.New("pending reset intent changed before request dispatch")
		}
		a.LastResetAttemptAt = dispatchAt
		threshold := effectiveAutoResetThreshold(a)
		if a.PendingThresholdPercent == nil {
			if a.AutoRequestID == requestID && a.AutoRequestThresholdPercent != nil {
				threshold = *a.AutoRequestThresholdPercent
			} else if !newPending {
				threshold = 1
			}
			v := threshold
			a.PendingThresholdPercent = &v
		}
		threshold = *a.PendingThresholdPercent
		a.LastResetThresholdPercent = &threshold
		return nil
	}); err != nil {
		if newPending {
			_ = c.withResetState(func(s *resetState) error {
				if a := s.Accounts[key]; a != nil && a.Pending != nil && a.Pending.RequestID == requestID && a.Pending.CreditID == creditID {
					a.Pending = nil
					a.PendingThresholdPercent = nil
				}
				return nil
			})
		}
		return nil, fmt.Errorf("could not persist reset dispatch; no request was sent (request ID %s): %w", requestID, err)
	}
	c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry {
		e.ResetAt = dispatchAt
		e.AttemptedAt = dispatchAt
		e.Usage = nil
		e.FetchedAt = 0
		e.LastError = "reset request outcome pending"
		return e
	})
	if err = ctx.Err(); err != nil {
		if newPending {
			_ = c.withResetState(func(s *resetState) error {
				if a := s.Accounts[key]; a != nil && a.Pending != nil && a.Pending.RequestID == requestID && a.Pending.CreditID == creditID {
					a.Pending = nil
					a.PendingThresholdPercent = nil
				}
				return nil
			})
		}
		return nil, fmt.Errorf("%w before reset request was sent; retry with request ID %s", err, requestID)
	}
	// From issuance onward, preserve a definitive provider response despite UI cancellation.
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	payload, _ := json.Marshal(struct {
		RedeemRequestID string `json:"redeem_request_id"`
		CreditID        string `json:"credit_id,omitempty"`
	}{requestID, creditID})
	endpoint := strings.TrimRight(c.CodexResetCreditsURL, "/") + "/consume"
	res, callErr := c.resetRequest(postCtx, http.MethodPost, endpoint, map[string]string{"Authorization": "Bearer " + r.AccessToken, "ChatGPT-Account-Id": r.AccountID, "Content-Type": "application/json"}, bytes.NewReader(payload))
	if callErr != nil {
		return nil, fmt.Errorf("reset outcome is unknown; retry with request ID %s: %w", requestID, callErr)
	}
	if res.Status == 429 {
		c.recordReset429(key, res)
		return nil, fmt.Errorf("reset redemption was rate limited; retry with request ID %s", requestID)
	}
	if !res.ok() {
		return nil, fmt.Errorf("reset redemption %d has unresolved outcome; retry with request ID %s: %s", res.Status, requestID, res.describe())
	}
	var wire resetPostWire
	if json.Unmarshal(res.Body, &wire) != nil || !validResetCode(wire.Code) {
		return nil, fmt.Errorf("reset redemption returned an unknown or malformed result; retry with request ID %s", requestID)
	}
	n := 0
	if wire.WindowsReset != nil {
		if *wire.WindowsReset < 0 {
			return nil, fmt.Errorf("reset redemption returned an invalid windows_reset; retry with request ID %s", requestID)
		}
		n = *wire.WindowsReset
	}
	out := &BankedResetRedemption{Code: wire.Code, WindowsReset: n, RequestID: requestID}
	if wire.Code == "nothing_to_reset" {
		out.Message = "There was nothing to reset."
	}
	if wire.Code == "no_credit" {
		out.Message = "No reset credit was available."
	}
	if wire.Code == "already_redeemed" {
		out.Message = "This reset request was already redeemed."
	}
	if wire.Code == "reset" {
		out.Message = fmt.Sprintf("Reset applied to %d usage window(s).", n)
	}
	if err := c.withResetState(func(s *resetState) error {
		a := resetAccount(s, key)
		a.Pending = nil
		a.PendingThresholdPercent = nil
		if a.Completed == nil {
			a.Completed = map[string]completedReset{}
		}
		var issuedThreshold *int
		if a.LastResetThresholdPercent != nil {
			v := *a.LastResetThresholdPercent
			issuedThreshold = &v
		}
		a.Completed[requestID] = completedReset{CreditID: creditID, Result: *out, ThresholdPercent: issuedThreshold}
		// Any confirmed spend must pass through a fresh below-threshold usage
		// observation before automation can spend again, including manual spends.
		a.AutoArmed = false
		a.AutoRequestID = requestID
		a.AutoCreditID = creditID
		a.AutoRequestThresholdPercent = nil
		a.AutoStatus = "reset result: " + out.Code
		a.Details = nil
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reset was processed but its receipt could not be saved; retry request %s: %w", requestID, err)
	}
	// Drop old usage while retaining all throttle state. A subsequent normal fetch is authoritative.
	c.cacheUpdate(key, func(e *cacheEntry) *cacheEntry {
		e.Usage = nil
		e.FetchedAt = 0
		e.LastError = "reset redeemed; usage refresh required"
		return e
	})
	return out, nil
}

func validResetCode(s string) bool {
	return s == "reset" || s == "nothing_to_reset" || s == "no_credit" || s == "already_redeemed"
}
func newUUID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// validateConfiguredResetURL protects malformed custom/test URLs before HTTP construction.
func (c *Config) validateConfiguredResetURL() error {
	u, e := url.Parse(c.CodexResetCreditsURL)
	if e != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("invalid Codex reset credits URL")
	}
	return nil
}

// resetRequest disables redirects so bearer credentials never follow an endpoint
// supplied redirect to another host.
func (c *Config) resetRequest(ctx context.Context, method, endpoint string, headers map[string]string, body io.Reader) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aiu/"+Version)
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New(Redact(err.Error()))
	}
	defer res.Body.Close()
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("reset endpoint response exceeds the size limit")
	}
	return &apiResponse{Status: res.StatusCode, Body: data, Header: res.Header}, nil
}
