package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// Record is one account's stored login. Times are Unix milliseconds, matching what
// Claude Code writes, so values copy across without conversion.
type Record struct {
	Provider              Provider       `json:"provider"`
	Email                 string         `json:"email"`
	Label                 string         `json:"label,omitempty"`
	AccessToken           string         `json:"accessToken,omitempty"`
	RefreshToken          string         `json:"refreshToken,omitempty"`
	IDToken               string         `json:"idToken,omitempty"`
	ExpiresAt             int64          `json:"expiresAt,omitempty"`
	RefreshTokenExpiresAt int64          `json:"refreshTokenExpiresAt,omitempty"`
	LastRefresh           int64          `json:"lastRefresh,omitempty"`
	Scopes                []string       `json:"scopes,omitempty"`
	SubscriptionType      string         `json:"subscriptionType,omitempty"`
	RateLimitTier         string         `json:"rateLimitTier,omitempty"`
	PlanType              string         `json:"planType,omitempty"`
	AccountID             string         `json:"accountId,omitempty"`
	UserID                string         `json:"userId,omitempty"`
	OrgUUID               string         `json:"orgUuid,omitempty"`
	OrgName               string         `json:"orgName,omitempty"`
	Profile               map[string]any `json:"profile,omitempty"`
	Source                string         `json:"source,omitempty"`
	CapturedAt            int64          `json:"capturedAt,omitempty"`
	UpdatedAt             int64          `json:"updatedAt,omitempty"`

	// Missing marks an index entry whose tokens are gone from the store.
	Missing bool `json:"-"`
}

// StoreKey is the name a record is stored and cached under. One address can hold
// both subscriptions, and — on Claude — several organizations with separate limits,
// so the provider and the organization are both part of the key.
func (r *Record) StoreKey() string { return storeKey(r.Provider, r.Email, r.OrgUUID) }

func storeKey(p Provider, email, org string) string {
	key := string(p) + ":" + email
	if org != "" {
		key += "#" + org
	}
	return key
}

// IsExpired reports whether the access token is within margin of expiry.
func (r *Record) IsExpired(now time.Time, margin time.Duration) bool {
	return r.ExpiresAt == 0 || r.ExpiresAt-now.UnixMilli() <= margin.Milliseconds()
}

// IsReadOnly is a login minted with only user:profile: it reads usage but cannot run
// inference, so it can never be switched to.
func (r *Record) IsReadOnly() bool {
	if r.Provider != Claude || len(r.Scopes) == 0 {
		return false
	}
	for _, s := range r.Scopes {
		if s == "user:inference" {
			return false
		}
	}
	return true
}

// IndexEntry is one tracked account; tokens live in the store, not here.
type IndexEntry struct {
	Email string `json:"email"`
	Label string `json:"label"`
	// Org is the Claude organization uuid, or the ChatGPT account id: the same
	// address can hold two of either, each with its own limits.
	Org      string   `json:"org,omitempty"`
	OrgName  string   `json:"orgName,omitempty"`
	Provider Provider `json:"provider"`
	AddedAt  string   `json:"addedAt,omitempty"`
}

// Describe names an account for an error message.
func (e *IndexEntry) Describe() string {
	if e.OrgName != "" {
		return string(e.Provider) + ":" + e.Email + " (" + e.OrgName + ")"
	}
	return string(e.Provider) + ":" + e.Email
}

// Index is the account list, in the order accounts were added.
type Index struct {
	Version  int           `json:"version"`
	Accounts []*IndexEntry `json:"accounts"`
}

// LoadIndex reads the account list; no file is an empty list.
func (c *Config) LoadIndex() (*Index, error) {
	idx := &Index{Version: 1}
	ok, err := readJSONFile(c.indexFile(), idx)
	if err != nil {
		return nil, fmt.Errorf("account index is unreadable (%v) — move it aside and re-add accounts; stored tokens are untouched", err)
	}
	if !ok || idx.Accounts == nil {
		idx.Accounts = []*IndexEntry{}
	}
	c.fillMissingOrgs(idx)
	return idx, nil
}

// fillMissingOrgs upgrades accounts stored before keys carried an organization. Until
// an entry knows its organization, a second organization on the same address would
// look like the same account and overwrite it. Best effort: a failure here leaves the
// account exactly as it was.
func (c *Config) fillMissingOrgs(idx *Index) {
	changed := false
	for _, e := range idx.Accounts {
		if e.Org != "" {
			continue
		}
		oldKey := storeKey(e.Provider, e.Email, "")
		r, err := c.tokenGet(oldKey)
		if err != nil || r == nil {
			continue
		}
		org, orgName := r.AccountID, ""
		if e.Provider == Claude {
			org, orgName = str(r.Profile["organizationUuid"]), str(r.Profile["organizationName"])
		}
		if org == "" {
			continue
		}
		r.OrgUUID, r.OrgName = org, orgName
		if err := c.tokenSet(storeKey(e.Provider, e.Email, org), r); err != nil {
			c.Warn("could not re-key " + e.Email + ": " + err.Error())
			continue
		}
		if err := c.tokenDelete(oldKey); err != nil {
			c.Warn("could not remove the old key for " + e.Email + ": " + err.Error())
			continue
		}
		e.Org, e.OrgName, changed = org, orgName, true
	}
	if changed {
		if err := c.saveIndex(idx); err != nil {
			c.Warn("could not save the account index: " + err.Error())
		}
	}
}

func (c *Config) saveIndex(idx *Index) error { return writePrivateJSON(c.indexFile(), idx) }

func (c *Config) tokenGet(key string) (*Record, error) {
	if c.UseKeychain {
		raw, ok, err := keychainRead(c.StoreService, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		var r Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, fmt.Errorf("stored token for %s is corrupt: %w", key, err)
		}
		return &r, nil
	}
	store := map[string]*Record{}
	if _, err := readTokenStore(c, &store); err != nil {
		return nil, err
	}
	return store[key], nil
}

func (c *Config) tokenSet(key string, r *Record) error {
	if c.UseKeychain {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return keychainWrite(c.StoreService, key, string(data))
	}
	return withFileLock(c.fileStore()+".lock", func() error {
		store := map[string]*Record{}
		if _, err := readTokenStore(c, &store); err != nil {
			return err
		}
		store[key] = r
		return writeTokenStore(c, store)
	})
}

func (c *Config) tokenDelete(key string) error {
	if c.UseKeychain {
		return keychainDelete(c.StoreService, key)
	}
	return withFileLock(c.fileStore()+".lock", func() error {
		store := map[string]*Record{}
		if _, err := readTokenStore(c, &store); err != nil {
			return err
		}
		delete(store, key)
		return writeTokenStore(c, store)
	})
}

// LoadRecords joins the index with the stored tokens.
func (c *Config) LoadRecords(idx *Index) ([]*Record, error) {
	out := make([]*Record, 0, len(idx.Accounts))
	for _, e := range idx.Accounts {
		r, err := c.tokenGet(storeKey(e.Provider, e.Email, e.Org))
		if err != nil {
			return nil, err
		}
		if r == nil {
			r = &Record{Missing: true}
		}
		r.Provider, r.Email, r.Label = e.Provider, e.Email, e.Label
		r.OrgUUID, r.OrgName = e.Org, e.OrgName
		out = append(out, r)
	}
	return out, nil
}
