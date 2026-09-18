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
	var email, org, orgName string
	profile := map[string]any{}
	if working.Provider == Codex {
		id := codexIdentity(working.IDToken, working.AccountID)
		if id.Email == "" {
			return nil, errors.New("the Codex login did not include an email address in its id_token")
		}
		email, org = id.Email, id.AccountID
		profile["emailAddress"] = email
	} else {
		var err error
		email, profile, err = c.fetchClaudeProfile(ctx, working.AccessToken)
		if err != nil {
			return nil, err
		}
		org, orgName = str(profile["organizationUuid"]), str(profile["organizationName"])
		if mergeClaudeJSON {
			// Claude Code's cached block is only the same account when the address and
			// the organization both match: one address can hold several organizations.
			cached := c.claudeCachedAccount()
			if str(cached["emailAddress"]) == email && (org == "" || str(cached["organizationUuid"]) == org) {
				for _, k := range profileFields {
					if v := cached[k]; v != nil {
						profile[k] = v
					}
				}
				org = firstNonEmpty(org, str(cached["organizationUuid"]))
				orgName = firstNonEmpty(orgName, str(cached["organizationName"]))
			}
		}
	}

	idx, err := c.LoadIndex()
	if err != nil {
		return nil, err
	}
	var existing *IndexEntry
	for _, e := range idx.Accounts {
		if e.Email == email && e.Provider == working.Provider && e.Org == org {
			existing = e
		}
	}
	if label == "" && existing != nil {
		label = existing.Label
	}
	if label == "" {
		label = autoLabel(idx, working.Provider, existing, email, orgName)
	} else if taken(idx, working.Provider, existing, label) {
		c.Warn(fmt.Sprintf("label %q is already used for this provider — pass --label to tell the two apart", label))
	}

	key := storeKey(working.Provider, email, org)
	previous, _ := c.tokenGet(key)
	now := c.now().UnixMilli()
	rec := *working
	rec.Email, rec.Label, rec.Profile, rec.UpdatedAt = email, label, profile, now
	rec.OrgUUID, rec.OrgName = org, orgName
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
		existing.Label, existing.Org, existing.OrgName = label, org, firstNonEmpty(orgName, existing.OrgName)
	} else {
		idx.Accounts = append(idx.Accounts, &IndexEntry{
			Email: email, Label: label, Org: org, OrgName: orgName,
			Provider: working.Provider, AddedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	if err := c.saveIndex(idx); err != nil {
		return nil, err
	}
	return &SavedAccount{Record: &rec, IsNew: existing == nil}, nil
}

// CaptureClaudeCode adds the account Claude Code is signed in as, sharing its token
// chain rather than minting a second grant.
func (c *Config) CaptureClaudeCode(ctx context.Context, label string) (*SavedAccount, error) {
	live, err := c.readClaudeCode()
	if err != nil {
		return nil, err
	}
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
	if working.IsExpired(c.now(), refreshMargin) {
		spent := working.RefreshToken
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
		if _, err := c.handBackClaude(spent, working); err != nil {
			return nil, fmt.Errorf("refreshed Claude Code's token but could not save its replacement: %w", err)
		}
	}
	saved, err := c.persistAccount(ctx, working, label, "claude-code", true)
	if err != nil {
		return nil, err
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
	if working.IsExpired(c.now(), refreshMargin) {
		spent := working.RefreshToken
		fresh, err := c.refreshCodex(ctx, working)
		if err != nil {
			return nil, err
		}
		working = fresh
		if _, err := c.handBackCodex(spent, working); err != nil {
			return nil, fmt.Errorf("refreshed Codex's token but could not save its replacement: %w", err)
		}
	}
	saved, err := c.persistAccount(ctx, working, label, "codex-cli", false)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// personalOrg matches what Claude calls an individual's own organization.
var personalOrg = regexp.MustCompile(`(?i)'s Organization$`)

// autoLabel names an account without asking: the address' local part, and when that is
// already taken by another organization, the organization itself. Whatever the browser
// signed in as decides the name, so `aiu login` never needs --label.
func autoLabel(idx *Index, p Provider, self *IndexEntry, email, orgName string) string {
	base, _, _ := strings.Cut(email, "@")
	if !taken(idx, p, self, base) {
		return base
	}
	candidate := base + "-personal"
	if orgName != "" && !personalOrg.MatchString(orgName) {
		if s := slug(orgName); s != "" {
			candidate = s
		}
	}
	if !taken(idx, p, self, candidate) {
		return candidate
	}
	for n := 2; ; n++ {
		numbered := fmt.Sprintf("%s-%d", candidate, n)
		if !taken(idx, p, self, numbered) {
			return numbered
		}
	}
}

// taken reports whether another account of this provider already uses the label.
func taken(idx *Index, p Provider, self *IndexEntry, label string) bool {
	for _, e := range idx.Accounts {
		if e != self && e.Provider == p && e.Label == label {
			return true
		}
	}
	return false
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// slug turns an organization name into a label: "Threefold Inc." → "threefold-inc".
func slug(name string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

var prefixed = regexp.MustCompile(`(?i)^(claude|codex):(.+)$`)

// FindAccount resolves an email or label. A claude:/codex: prefix (or provider)
// narrows the search; a name tracked for both providers is an error, not a guess.
// The full key a listing prints — provider:email#organization — also resolves, which
// is what front ends pass when one address holds several organizations.
func (c *Config) FindAccount(target string, provider Provider) (*Index, *IndexEntry, error) {
	name := target
	if m := prefixed.FindStringSubmatch(target); m != nil {
		provider, name = Provider(strings.ToLower(m[1])), m[2]
	}
	name, org, hasOrg := strings.Cut(name, "#")
	idx, err := c.LoadIndex()
	if err != nil {
		return nil, nil, err
	}
	if hasOrg {
		for _, e := range idx.Accounts {
			if e.Email == name && e.Org == org && (provider == "" || e.Provider == provider) {
				return idx, e, nil
			}
		}
		return nil, nil, fmt.Errorf("no tracked account matches %q", target)
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
				names[i] = e.Label + " → " + e.Describe()
			}
			return nil, fmt.Errorf("%q matches %d accounts (%s) — use the label, or claude:/codex:", name, len(found), strings.Join(names, ", "))
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
	if err := c.tokenDelete(storeKey(e.Provider, e.Email, e.Org)); err != nil {
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
	c.cacheDelete(storeKey(e.Provider, e.Email, e.Org))
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
		id := live.identity()
		out[Codex] = &LiveMatch{Email: id.Email, Org: id.AccountID, Verified: id.Email != ""}
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
	// The entry names one account; a record matches it by address and organization.
	find := func(rs []*Record, e *IndexEntry) *Record {
		for _, r := range rs {
			if r.Provider == e.Provider && r.Email == e.Email && r.OrgUUID == e.Org && !r.Missing {
				return r
			}
		}
		return nil
	}
	tracked := func(rs []*Record, m LiveMatch, p Provider) bool {
		for _, r := range rs {
			if r.Provider == p && !r.Missing && m.Is(r) {
				return true
			}
		}
		return false
	}
	res := &SwitchResult{Entry: e}

	if e.Provider == Codex {
		live, m, synced := c.SyncCodex(records, true)
		rec := find(synced, e)
		if rec == nil {
			return nil, fmt.Errorf("token for %s not found — sign in again for it", e.Describe())
		}
		if live != nil && m.Is(rec) && live.refreshToken() == rec.RefreshToken {
			res.AlreadyActive = true
			return res, nil
		}
		if live != nil && !tracked(synced, m, Codex) {
			res.UntrackedReplaced = firstNonEmpty(m.Email, "an unidentified account")
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
	rec := find(synced, e)
	if rec != nil && m.Verified && m.Is(rec) {
		res.AlreadyActive = true
		return res, nil
	}
	if live != nil && !(m.Verified && tracked(synced, m, Claude)) {
		res.UntrackedReplaced = firstNonEmpty(m.Email, "an unidentified account")
	}
	if rec == nil {
		return nil, fmt.Errorf("token for %s not found — sign in again for it", e.Describe())
	}
	if rec.IsReadOnly() {
		return nil, fmt.Errorf("%s was added read-only and cannot run Claude Code — re-add it with a full login to switch to it", e.Describe())
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
