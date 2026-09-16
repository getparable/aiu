package core

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Fixed path, not a PATH lookup: this binary handles every token on the machine.
const securityBin = "/usr/bin/security"

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
	Profile               map[string]any `json:"profile,omitempty"`
	Source                string         `json:"source,omitempty"`
	CapturedAt            int64          `json:"capturedAt,omitempty"`
	UpdatedAt             int64          `json:"updatedAt,omitempty"`

	// Missing marks an index entry whose tokens are gone from the store.
	Missing bool `json:"-"`
}

// StoreKey is the name a record is stored and cached under. Codex keys are
// namespaced because one address can hold both subscriptions.
func (r *Record) StoreKey() string { return storeKey(r.Provider, r.Email) }

func storeKey(p Provider, email string) string { return string(p) + ":" + email }

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
	Email    string   `json:"email"`
	Label    string   `json:"label"`
	Provider Provider `json:"provider"`
	AddedAt  string   `json:"addedAt,omitempty"`
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
	return idx, nil
}

func (c *Config) saveIndex(idx *Index) error { return writePrivateJSON(c.indexFile(), idx) }

func (c *Config) tokenGet(key string) (*Record, error) {
	if c.UseKeychain {
		raw, ok := keychainRead(c.StoreService, key)
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
	if _, err := readJSONFile(c.fileStore(), &store); err != nil {
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
	store := map[string]*Record{}
	if _, err := readJSONFile(c.fileStore(), &store); err != nil {
		return err
	}
	store[key] = r
	return writePrivateJSON(c.fileStore(), store)
}

func (c *Config) tokenDelete(key string) error {
	if c.UseKeychain {
		keychainDelete(c.StoreService, key)
		return nil
	}
	store := map[string]*Record{}
	if _, err := readJSONFile(c.fileStore(), &store); err != nil {
		return err
	}
	delete(store, key)
	return writePrivateJSON(c.fileStore(), store)
}

// LoadRecords joins the index with the stored tokens.
func (c *Config) LoadRecords(idx *Index) ([]*Record, error) {
	out := make([]*Record, 0, len(idx.Accounts))
	for _, e := range idx.Accounts {
		r, err := c.tokenGet(storeKey(e.Provider, e.Email))
		if err != nil {
			return nil, err
		}
		if r == nil {
			r = &Record{Missing: true}
		}
		r.Provider, r.Email, r.Label = e.Provider, e.Email, e.Label
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------- keychain

func security(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(securityBin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// `security -w` prints a secret as hex when it holds bytes it will not print raw.
func decodeKeychainSecret(raw string) string {
	if len(raw) < 2 || len(raw)%2 != 0 {
		return raw
	}
	b, err := hex.DecodeString(raw)
	if err != nil || !utf8.Valid(b) {
		return raw
	}
	return string(b)
}

func keychainRead(service, account string) (string, bool) {
	args := []string{"find-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	out, _, err := security(append(args, "-w")...)
	if err != nil {
		return "", false
	}
	return decodeKeychainSecret(strings.TrimSuffix(out, "\n")), true
}

var acctPattern = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)

func keychainAccountName(service string) string {
	out, _, err := security("find-generic-password", "-s", service)
	if err != nil {
		return ""
	}
	if m := acctPattern.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// keychainWrite passes the secret as an argument, briefly visible to same-user
// processes via ps. The stdin form truncates at 128 bytes, far below a token record,
// and any same-user process can already read the item back through `security`.
func keychainWrite(service, account, secret string) error {
	_, stderr, err := security("add-generic-password", "-U", "-s", service, "-a", account, "-w", secret)
	if err != nil {
		return errors.New("keychain write failed: " + Redact(strings.TrimSpace(stderr)))
	}
	return nil
}

func keychainDelete(service, account string) {
	_, _, _ = security("delete-generic-password", "-s", service, "-a", account)
}
