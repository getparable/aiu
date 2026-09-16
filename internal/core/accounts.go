package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SavedAccount is the outcome of adding or re-adding an account.
type SavedAccount struct {
	Record *Record
	IsNew  bool
}

// persistAccount identifies a token set, then stores it and indexes the account.
func (c *Config) persistAccount(ctx context.Context, working *Record, label, source string, mergeClaudeJSON bool) (*SavedAccount, error) {
	var email string
	profile := map[string]any{}
	if working.Provider == Codex {
		id := codexIdentity(working.IDToken, working.AccountID)
		if id.Email == "" {
			return nil, errors.New("the Codex login did not include an email address in its id_token")
		}
		email = id.Email
		profile["emailAddress"] = email
	} else {
		var err error
		email, profile, err = c.fetchClaudeProfile(ctx, working.AccessToken)
		if err != nil {
			return nil, err
		}
		if mergeClaudeJSON {
			if cached := c.claudeCachedAccount(); str(cached["emailAddress"]) == email {
				for _, k := range profileFields {
					if v := cached[k]; v != nil {
						profile[k] = v
					}
				}
			}
		}
	}

	idx, err := c.LoadIndex()
	if err != nil {
		return nil, err
	}
	var existing *IndexEntry
	for _, e := range idx.Accounts {
		if e.Email == email && e.Provider == working.Provider {
			existing = e
		}
	}
	if label == "" && existing != nil {
		label = existing.Label
	}
	if label == "" {
		label, _, _ = strings.Cut(email, "@")
	}
	for _, e := range idx.Accounts {
		if e != existing && e.Provider == working.Provider && e.Label == label {
			c.Warn(fmt.Sprintf("label %q is also used by %s — use the email address to refer to either account", label, e.Email))
		}
	}

	key := storeKey(working.Provider, email)
	previous, _ := c.tokenGet(key)
	now := c.now().UnixMilli()
	rec := *working
	rec.Email, rec.Label, rec.Profile, rec.UpdatedAt = email, label, profile, now
	rec.Source = firstNonEmpty(source, rec.Source)
	rec.CapturedAt = now
	if previous != nil {
		if previous.CapturedAt > 0 {
			rec.CapturedAt = previous.CapturedAt
		}
		rec.SubscriptionType = firstNonEmpty(rec.SubscriptionType, previous.SubscriptionType)
		rec.RateLimitTier = firstNonEmpty(rec.RateLimitTier, previous.RateLimitTier)
	}
	if err := c.tokenSet(key, &rec); err != nil {
		return nil, err
	}
	if existing != nil {
		existing.Label = label
	} else {
		idx.Accounts = append(idx.Accounts, &IndexEntry{Email: email, Label: label, Provider: working.Provider, AddedAt: time.Now().UTC().Format(time.RFC3339)})
	}
	if err := c.saveIndex(idx); err != nil {
		return nil, err
	}
	return &SavedAccount{Record: &rec, IsNew: existing == nil}, nil
}

// CaptureClaudeCode adds the account Claude Code is signed in as, sharing its token
// chain rather than minting a second grant.
func (c *Config) CaptureClaudeCode(ctx context.Context, label string) (*SavedAccount, error) {
	live := c.ReadClaudeCode()
	if live == nil {
		return nil, errors.New("no Claude Code login found — sign in with `claude` first, or use `aiu login`")
	}
	working := &Record{
		Provider:              Claude,
		AccessToken:           live.accessToken(),
		RefreshToken:          live.refreshToken(),
		ExpiresAt:             live.int64Field("expiresAt"),
		RefreshTokenExpiresAt: live.int64Field("refreshTokenExpiresAt"),
		SubscriptionType:      str(live.OAuth["subscriptionType"]),
		RateLimitTier:         str(live.OAuth["rateLimitTier"]),
	}
	if scopes, ok := live.OAuth["scopes"].([]any); ok {
		for _, s := range scopes {
			working.Scopes = append(working.Scopes, str(s))
		}
	}
	rotated := false
	if working.IsExpired(c.now(), refreshMargin) {
		fresh, err := c.refreshClaude(ctx, working.RefreshToken)
		if err != nil {
			return nil, err
		}
		working.AccessToken, working.RefreshToken, working.ExpiresAt = fresh.AccessToken, fresh.RefreshToken, fresh.ExpiresAt
		if fresh.RefreshTokenExpiresAt > 0 {
			working.RefreshTokenExpiresAt = fresh.RefreshTokenExpiresAt
		}
		if fresh.Scopes != nil {
			working.Scopes = fresh.Scopes
		}
		rotated = true
	}
	saved, err := c.persistAccount(ctx, working, label, "claude-code", true)
	if err != nil {
		return nil, err
	}
	if rotated {
		if _, err := c.writeClaudeCode(live, claudeTokenPatch(working), false); err != nil {
			c.Warn(fmt.Sprintf("captured %s but could not write the rotated token back to Claude Code: %v", saved.Record.Email, err))
		}
	}
	return saved, nil
}

// CaptureCodex adds the account Codex is signed in as.
func (c *Config) CaptureCodex(ctx context.Context, label string) (*SavedAccount, error) {
	live := c.ReadCodexAuth()
	if live == nil {
		return nil, fmt.Errorf("no Codex login found (%s) — run `codex login` first, or use `aiu login --codex`", c.codexAuthFile())
	}
	if mode := str(live.Doc["auth_mode"]); mode != "" && mode != "chatgpt" {
		return nil, fmt.Errorf("Codex is signed in with %s, not a ChatGPT login — only ChatGPT logins carry usage limits", mode)
	}
	working := codexRecordFromLive(live)
	rotated := false
	if working.IsExpired(c.now(), refreshMargin) {
		fresh, err := c.refreshCodex(ctx, working)
		if err != nil {
			return nil, err
		}
		working, rotated = fresh, true
	}
	saved, err := c.persistAccount(ctx, working, label, "codex-cli", false)
	if err != nil {
		return nil, err
	}
	if rotated {
		if _, err := c.writeCodexAuth(live, working); err != nil {
			c.Warn(fmt.Sprintf("captured %s but could not write the rotated token back to Codex: %v", saved.Record.Email, err))
		}
	}
	return saved, nil
}

var prefixed = regexp.MustCompile(`(?i)^(claude|codex):(.+)$`)

// FindAccount resolves an email or label. A claude:/codex: prefix (or provider)
// narrows the search; a name tracked for both providers is an error, not a guess.
func (c *Config) FindAccount(target string, provider Provider) (*Index, *IndexEntry, error) {
	name := target
	if m := prefixed.FindStringSubmatch(target); m != nil {
		provider, name = Provider(strings.ToLower(m[1])), m[2]
	}
	idx, err := c.LoadIndex()
	if err != nil {
		return nil, nil, err
	}
	pick := func(match func(*IndexEntry) bool) (*IndexEntry, error) {
		var found []*IndexEntry
		for _, e := range idx.Accounts {
			if (provider == "" || e.Provider == provider) && match(e) {
				found = append(found, e)
			}
		}
		if len(found) > 1 {
			names := make([]string, len(found))
			for i, e := range found {
				names[i] = string(e.Provider) + ":" + e.Email
			}
			return nil, fmt.Errorf("%q matches %d accounts (%s) — use claude:/codex: or the email address", name, len(found), strings.Join(names, ", "))
		}
		if len(found) == 1 {
			return found[0], nil
		}
		return nil, nil
	}
	e, err := pick(func(e *IndexEntry) bool { return e.Email == name })
	if err == nil && e == nil {
		e, err = pick(func(e *IndexEntry) bool { return e.Label == name })
	}
	if err != nil {
		return nil, nil, err
	}
	if e == nil {
		return nil, nil, fmt.Errorf("no tracked account matches %q", target)
	}
	return idx, e, nil
}

// RemoveAccount forgets an account: its tokens, its index entry and its cached usage.
func (c *Config) RemoveAccount(target string, provider Provider) (*IndexEntry, error) {
	idx, e, err := c.FindAccount(target, provider)
	if err != nil {
		return nil, err
	}
	if err := c.tokenDelete(storeKey(e.Provider, e.Email)); err != nil {
		return nil, err
	}
	kept := idx.Accounts[:0]
	for _, a := range idx.Accounts {
		if a != e {
			kept = append(kept, a)
		}
	}
	idx.Accounts = kept
	if err := c.saveIndex(idx); err != nil {
		return nil, err
	}
	c.cacheDelete(storeKey(e.Provider, e.Email))
	return e, nil
}

// DescribeLive says who each CLI is signed in as. verify may use the network for an
// unrecognised Claude Code token; Codex is always local.
func (c *Config) DescribeLive(ctx context.Context, verify bool) (map[Provider]*LiveMatch, error) {
	idx, err := c.LoadIndex()
	if err != nil {
		return nil, err
	}
	records, err := c.LoadRecords(idx)
	if err != nil {
		return nil, err
	}
	out := map[Provider]*LiveMatch{}
	if live := c.ReadClaudeCode(); live != nil {
		m := matchClaudeLive(c, live, records)
		if verify {
			m = c.resolveClaudeLive(ctx, live, records)
		}
		out[Claude] = &m
	}
	if live := c.ReadCodexAuth(); live != nil {
		out[Codex] = &LiveMatch{Email: live.LiveEmail(), Verified: true}
	}
	return out, nil
}

// SwitchResult reports what SwitchAccount did.
type SwitchResult struct {
	Entry             *IndexEntry
	AlreadyActive     bool
	UntrackedReplaced string // the previous login, when no tracked record held it
	UpdatedGlobal     bool   // .claude.json's account block was rewritten
}

// SwitchAccount points Claude Code or Codex at a tracked account. Running sessions
// pick it up the next time they re-read their credentials.
func (c *Config) SwitchAccount(ctx context.Context, target string, provider Provider) (*SwitchResult, error) {
	idx, e, err := c.FindAccount(target, provider)
	if err != nil {
		return nil, err
	}
	records, err := c.LoadRecords(idx)
	if err != nil {
		return nil, err
	}
	find := func(rs []*Record, p Provider, email string) *Record {
		for _, r := range rs {
			if r.Provider == p && r.Email == email && !r.Missing {
				return r
			}
		}
		return nil
	}
	res := &SwitchResult{Entry: e}

	if e.Provider == Codex {
		live, liveEmail, synced := c.SyncCodex(records, true)
		rec := find(synced, Codex, e.Email)
		if rec == nil {
			return nil, fmt.Errorf("token for %s not found — sign in again for it", e.Email)
		}
		if live != nil && liveEmail == e.Email && live.refreshToken() == rec.RefreshToken {
			res.AlreadyActive = true
			return res, nil
		}
		if live != nil && find(synced, Codex, liveEmail) == nil {
			res.UntrackedReplaced = firstNonEmpty(liveEmail, "an unidentified account")
		}
		if rec, err = c.ensureFresh(ctx, rec, nil, nil, false); err != nil {
			return nil, err
		}
		if _, err := c.writeCodexAuth(live, rec); err != nil {
			return nil, err
		}
		res.UpdatedGlobal = true
		return res, nil
	}

	live, m, synced := c.SyncClaude(ctx, records, true)
	if m.Verified && m.Email == e.Email {
		res.AlreadyActive = true
		return res, nil
	}
	if live != nil && !(m.Verified && find(synced, Claude, m.Email) != nil) {
		res.UntrackedReplaced = firstNonEmpty(m.Email, "an unidentified account")
	}
	rec := find(synced, Claude, e.Email)
	if rec == nil {
		return nil, fmt.Errorf("token for %s not found — sign in again for it", e.Email)
	}
	if rec.IsReadOnly() {
		return nil, fmt.Errorf("%s was added read-only and cannot run Claude Code — re-add it with a full login to switch to it", e.Email)
	}
	if rec, err = c.ensureFresh(ctx, rec, nil, nil, false); err != nil {
		return nil, err
	}
	patch := claudeTokenPatch(rec)
	patch["scopes"] = rec.Scopes
	patch["subscriptionType"] = rec.SubscriptionType
	patch["rateLimitTier"] = rec.RateLimitTier
	if _, err := c.writeClaudeCode(live, patch, true); err != nil {
		return nil, err
	}
	profile := map[string]any{}
	for k, v := range rec.Profile {
		profile[k] = v
	}
	profile["emailAddress"] = rec.Email
	updated, err := c.updateClaudeGlobalAccount(profile)
	if err != nil {
		c.Warn("switched, but could not update .claude.json: " + err.Error())
	}
	res.UpdatedGlobal = updated
	return res, nil
}
