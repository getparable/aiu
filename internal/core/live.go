package core

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"time"
)

// All AIU configurations for this user lock the same CLI destination.
func (c *Config) liveLockPath(provider Provider) string {
	if provider == Claude {
		return filepath.Join(c.ClaudeDir, ".aiu-credentials.lock")
	}
	return filepath.Join(c.CodexHome, ".aiu-auth.lock")
}

// LiveClaude is the login Claude Code holds right now. Doc keeps every top-level key
// (e.g. mcpOAuth) so a write-back never drops what Claude Code stored beside it.
type LiveClaude struct {
	Doc   map[string]json.RawMessage
	OAuth map[string]any

	fromFile bool
	path     string
	service  string
	account  string
}

func (l *LiveClaude) accessToken() string  { return str(l.OAuth["accessToken"]) }
func (l *LiveClaude) refreshToken() string { return str(l.OAuth["refreshToken"]) }
func (l *LiveClaude) int64Field(key string) int64 {
	n, _ := num(l.OAuth[key])
	return int64(n)
}

// ReadClaudeCode returns Claude Code's current login, or nil when it has none.
func (c *Config) ReadClaudeCode() *LiveClaude {
	live, _ := c.readClaudeCode()
	return live
}

func (c *Config) readClaudeCode() (*LiveClaude, error) {
	file := filepath.Join(c.ClaudeDir, ".credentials.json")
	if data, err := os.ReadFile(file); err == nil {
		if l := parseLiveClaude(data); l != nil {
			l.fromFile, l.path = true, file
			return l, nil
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	raw, account, ok, err := readKeychainItem(c.ClaudeService, "")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	l := parseLiveClaude([]byte(raw))
	if l == nil {
		return nil, nil
	}
	l.service = c.ClaudeService
	if account == "" {
		return nil, errors.New("Claude Code Keychain item has no account attribute")
	}
	l.account = account
	return l, nil
}

func parseLiveClaude(data []byte) *LiveClaude {
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var oauth map[string]any
	if json.Unmarshal(doc["claudeAiOauth"], &oauth) != nil || str(oauth["accessToken"]) == "" {
		return nil
	}
	return &LiveClaude{Doc: doc, OAuth: oauth}
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "claude-code-user"
}

// writeClaudeCode merges patch into Claude Code's oauth block (or replaces the block)
// and writes it back where it came from. live may be nil when Claude Code has no login.
func (c *Config) writeClaudeCode(live *LiveClaude, patch map[string]any, replace bool) (*LiveClaude, error) {
	var written *LiveClaude
	err := withFileLock(c.liveLockPath(Claude), func() error {
		// Use the newest document so another AIU write cannot lose sibling fields.
		current, readErr := c.readClaudeCode()
		if readErr != nil {
			return readErr
		}
		var writeErr error
		written, writeErr = c.writeClaudeCodeUnlocked(current, patch, replace)
		return writeErr
	})
	return written, err
}

func (c *Config) writeClaudeCodeUnlocked(live *LiveClaude, patch map[string]any, replace bool) (*LiveClaude, error) {
	next := &LiveClaude{Doc: map[string]json.RawMessage{}, OAuth: map[string]any{}}
	if live != nil {
		for k, v := range live.Doc {
			next.Doc[k] = v
		}
		if !replace {
			for k, v := range live.OAuth {
				next.OAuth[k] = v
			}
		}
		next.fromFile, next.path, next.service, next.account = live.fromFile, live.path, live.service, live.account
	} else if runtime.GOOS == "darwin" {
		next.service, next.account = c.ClaudeService, currentUser()
	} else {
		next.fromFile, next.path = true, filepath.Join(c.ClaudeDir, ".credentials.json")
	}
	for k, v := range patch {
		if v == nil || reflect.ValueOf(v).IsZero() {
			continue
		}
		next.OAuth[k] = v
	}
	oauth, err := json.Marshal(next.OAuth)
	if err != nil {
		return nil, err
	}
	next.Doc["claudeAiOauth"] = oauth
	data, err := json.Marshal(next.Doc)
	if err != nil {
		return nil, err
	}
	if next.fromFile {
		err = writePrivateFile(next.path, data, false)
	} else {
		err = keychainWrite(next.service, next.account, string(data))
	}
	return next, err
}

// handBackClaude only replaces the refresh token this operation spent. The read
// and write share AIU's live credential lock. External writers do not use that
// lock, so the reread narrows their race without eliminating it.
func (c *Config) handBackClaude(spent string, r *Record) (bool, error) {
	var changed bool
	err := withFileLock(c.liveLockPath(Claude), func() error {
		live, readErr := c.readClaudeCode()
		if readErr != nil {
			return readErr
		}
		if live == nil || live.refreshToken() != spent {
			return nil
		}
		_, err := c.writeClaudeCodeUnlocked(live, claudeTokenPatch(r), false)
		changed = err == nil
		return err
	})
	return changed, err
}

func claudeTokenPatch(r *Record) map[string]any {
	return map[string]any{
		"accessToken":           r.AccessToken,
		"refreshToken":          r.RefreshToken,
		"expiresAt":             r.ExpiresAt,
		"refreshTokenExpiresAt": r.RefreshTokenExpiresAt,
	}
}

// profileFields are the oauthAccount keys switch rewrites in .claude.json.
var profileFields = []string{
	"accountUuid", "emailAddress", "displayName", "fullName", "organizationUuid",
	"organizationName", "organizationType", "organizationRole", "organizationRateLimitTier", "billingType",
}

// ClaudeCachedEmail is the address .claude.json says Claude Code is signed in as.
// It is a hint only: Claude Code does not rewrite it on every token change.
func (c *Config) ClaudeCachedEmail() string {
	email, _ := c.claudeCachedAccountIdentity()
	return email
}

// claudeCachedAccountIdentity also returns the organization, which is what tells two
// logins on one address apart.
func (c *Config) claudeCachedAccountIdentity() (string, string) {
	acct := c.claudeCachedAccount()
	return str(acct["emailAddress"]), str(acct["organizationUuid"])
}

func (c *Config) claudeCachedAccount() map[string]any {
	var cfg map[string]any
	if ok, _ := readJSONFile(c.ClaudeGlobalConfig, &cfg); !ok {
		return nil
	}
	return obj(cfg["oauthAccount"])
}

// updateClaudeGlobalAccount rewrites the oauthAccount block in .claude.json, keeping
// every other key and the file's mode. The file is Claude Code's, so it is replaced
// atomically rather than rewritten in place.
func (c *Config) updateClaudeGlobalAccount(profile map[string]any) (bool, error) {
	data, err := os.ReadFile(c.ClaudeGlobalConfig)
	if err != nil {
		return false, nil
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(data, &cfg) != nil {
		return false, nil
	}
	var acct map[string]any
	if json.Unmarshal(cfg["oauthAccount"], &acct) != nil || acct == nil {
		return false, nil
	}
	for _, k := range profileFields {
		if v, ok := profile[k]; ok && v != nil {
			acct[k] = v
		}
	}
	acct["profileFetchedAt"] = c.now().UnixMilli()
	raw, err := json.Marshal(acct)
	if err != nil {
		return false, err
	}
	cfg["oauthAccount"] = raw
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeAtomicFile(c.ClaudeGlobalConfig, out, false, true); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------- codex

// LiveCodex is Codex's auth.json (ChatGPT login mode), with every key preserved.
type LiveCodex struct {
	Doc    map[string]any
	Tokens map[string]any
}

func (l *LiveCodex) accessToken() string  { return str(l.Tokens["access_token"]) }
func (l *LiveCodex) refreshToken() string { return str(l.Tokens["refresh_token"]) }

// ReadCodexAuth returns Codex's ChatGPT login, or nil when it has none.
func (c *Config) ReadCodexAuth() *LiveCodex {
	var doc map[string]any
	if ok, _ := readJSONFile(c.codexAuthFile(), &doc); !ok {
		return nil
	}
	tokens := obj(doc["tokens"])
	if str(tokens["access_token"]) == "" {
		return nil
	}
	return &LiveCodex{Doc: doc, Tokens: tokens}
}

// CodexIdentity is who a Codex token set belongs to, read from its id_token — no
// network round trip needed.
type CodexIdentity struct {
	Email, AccountID, PlanType, UserID string
}

func codexIdentity(idToken, accountID string) CodexIdentity {
	claims := jwtClaims(idToken)
	auth := obj(claims["https://api.openai.com/auth"])
	return CodexIdentity{
		Email:     firstNonEmpty(str(claims["email"]), str(obj(claims["https://api.openai.com/profile"])["email"])),
		AccountID: firstNonEmpty(accountID, str(auth["chatgpt_account_id"])),
		PlanType:  str(auth["chatgpt_plan_type"]),
		UserID:    firstNonEmpty(str(auth["chatgpt_user_id"]), str(auth["user_id"])),
	}
}

// identity is who Codex is signed in as, read from its id_token.
func (l *LiveCodex) identity() CodexIdentity {
	return codexIdentity(str(l.Tokens["id_token"]), str(l.Tokens["account_id"]))
}

// LiveEmail is the address Codex is signed in as.
func (l *LiveCodex) LiveEmail() string { return l.identity().Email }

func codexRecordFromLive(l *LiveCodex) *Record {
	id := codexIdentity(str(l.Tokens["id_token"]), str(l.Tokens["account_id"]))
	r := &Record{
		Provider:     Codex,
		AccessToken:  l.accessToken(),
		RefreshToken: l.refreshToken(),
		IDToken:      str(l.Tokens["id_token"]),
		AccountID:    id.AccountID,
		PlanType:     id.PlanType,
		UserID:       id.UserID,
		Email:        id.Email,
	}
	if t, err := time.Parse(time.RFC3339Nano, str(l.Doc["last_refresh"])); err == nil {
		r.LastRefresh = t.UnixMilli()
	}
	r.ExpiresAt = jwtExpiryMs(r.AccessToken)
	if r.ExpiresAt == 0 && r.LastRefresh > 0 {
		r.ExpiresAt = r.LastRefresh + codexTokenLifetime.Milliseconds()
	}
	return r
}

// writeCodexAuth stores a token set in auth.json the way `codex login` does, keeping
// every other key. The directory is Codex's, so its mode is left alone.
func (c *Config) writeCodexAuth(live *LiveCodex, r *Record) (*LiveCodex, error) {
	var written *LiveCodex
	err := withFileLock(c.liveLockPath(Codex), func() error {
		var writeErr error
		written, writeErr = c.writeCodexAuthUnlocked(c.ReadCodexAuth(), r)
		return writeErr
	})
	return written, err
}

func (c *Config) writeCodexAuthUnlocked(live *LiveCodex, r *Record) (*LiveCodex, error) {
	doc := map[string]any{}
	if live != nil {
		for k, v := range live.Doc {
			doc[k] = v
		}
	}
	doc["auth_mode"] = "chatgpt"
	if _, ok := doc["OPENAI_API_KEY"]; !ok {
		doc["OPENAI_API_KEY"] = nil
	}
	tokens := map[string]any{"access_token": r.AccessToken}
	for k, v := range map[string]string{"id_token": r.IDToken, "refresh_token": r.RefreshToken, "account_id": r.AccountID} {
		if v != "" {
			tokens[k] = v
		}
	}
	doc["tokens"] = tokens
	last := r.LastRefresh
	if last == 0 {
		last = c.now().UnixMilli()
	}
	doc["last_refresh"] = msToTime(last).UTC().Format("2006-01-02T15:04:05.000Z")
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writePrivateFile(c.codexAuthFile(), append(data, '\n'), false); err != nil {
		return nil, err
	}
	return &LiveCodex{Doc: doc, Tokens: tokens}, nil
}

func (c *Config) handBackCodex(spent string, r *Record) (bool, error) {
	var changed bool
	err := withFileLock(c.liveLockPath(Codex), func() error {
		live := c.ReadCodexAuth()
		if live == nil || live.refreshToken() != spent {
			return nil
		}
		_, err := c.writeCodexAuthUnlocked(live, r)
		changed = err == nil
		return err
	})
	return changed, err
}
