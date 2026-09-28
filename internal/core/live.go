package core

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
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

// claudeKeychainRead is the only way AIU reads Claude Code's Keychain item;
// tests swap it to prove a path never touches it.
var claudeKeychainRead = readClaudeKeychain

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
	raw, account, ok, err := claudeKeychainRead(c.ClaudeService)
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

var claudeAccountName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// currentUser is the account Claude Code files a new login under, and the one
// it reads back: $USER, else the OS username, else a fixed placeholder.
func currentUser() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if !claudeAccountName.MatchString(name) {
		return "claude-code-user"
	}
	return name
}

// writeClaudeCode merges patch into Claude Code's oauth block (or replaces the block)
// and writes it back where it came from. live may be nil when Claude Code has no login.
func (c *Config) writeClaudeCode(live *LiveClaude, patch map[string]any, replace bool) (*LiveClaude, error) {
	var written *LiveClaude
	err := c.withClaudeRefreshLock(func() error {
		return withFileLock(c.liveLockPath(Claude), func() error {
			// Use the newest document so another AIU write cannot lose sibling fields.
			current, readErr := c.readClaudeCode()
			if readErr != nil {
				return readErr
			}
			var writeErr error
			written, writeErr = c.writeClaudeCodeUnlocked(current, patch, replace)
			return writeErr
		})
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
		err = writeClaudeKeychain(next.service, next.account, string(data))
	}
	return next, err
}

// handBackClaudeHeld only replaces the refresh token this operation spent. The
// caller holds Claude Code's refresh lock, which is not reentrant; this adds
// AIU's live credential lock around the reread and write. Claude Code takes its
// lock only to refresh; its login, logout and other credential writes do not,
// so the reread narrows those races without eliminating them.
func (c *Config) handBackClaudeHeld(spent string, r *Record) (bool, error) {
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

// Claude Code serializes its OAuth refresh with proper-lockfile directory locks
// (verified in 2.1.284): <login dir>/.oauth_refresh.lock, then the legacy
// <realpath(login dir)>.lock. A lock whose mtime is a minute old is stale, so
// the holder touches it every 5s. Without an owner record Claude Code never
// takes over a live-looking lock; it waits for it to go stale.
const (
	claudeLockStale = time.Minute
	claudeLockTouch = 5 * time.Second
)

var errClaudeRefreshing = errors.New("Claude Code is refreshing its login; try again shortly")

// withClaudeRefreshLock runs fn holding Claude Code's refresh locks, so AIU never
// spends or hands back a token while Claude Code is refreshing. With no Claude
// Code login directory there is nothing to race, and fn runs unlocked.
func (c *Config) withClaudeRefreshLock(fn func() error) error {
	if st, err := os.Stat(c.ClaudeDir); err != nil || !st.IsDir() {
		return fn()
	}
	legacy := c.ClaudeDir
	if real, err := filepath.EvalSymlinks(c.ClaudeDir); err == nil {
		legacy = real
	}
	deadline := time.Now().Add(lockWait)
	var held []*heldDirLock
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].release()
		}
	}()
	for _, path := range []string{filepath.Join(c.ClaudeDir, ".oauth_refresh.lock"), legacy + ".lock"} {
		lock, err := acquireDirLock(path, c.liveLockPath(Claude), deadline)
		if err != nil {
			return err
		}
		held = append(held, lock)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(claudeLockTouch)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				for _, lock := range held {
					lock.touch(now)
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	return fn()
}

// heldDirLock is a proper-lockfile lock this process won, identified by its
// directory and the mtime this process last stamped on it. A lock can go stale
// while its holder is paused (a sleeping Mac), and Claude Code may then rightly
// replace it; touching or removing by path alone would refresh or delete Claude
// Code's lock. So every touch and release first checks the lock is still ours,
// the way proper-lockfile detects a compromised lock.
type heldDirLock struct {
	path string
	info fs.FileInfo
}

func (l *heldDirLock) owned() bool {
	now, err := os.Stat(l.path)
	return err == nil && os.SameFile(l.info, now) && now.ModTime().Equal(l.info.ModTime())
}

// touchLockPath stamps a lock directory; tests swap it to interleave a takeover.
var touchLockPath = os.Chtimes

// touch keeps an owned lock fresh; a lock someone else now holds is left alone.
// The new stamp is adopted only on the directory this process holds: if Claude
// Code replaced it between the check and the touch, the old identity is kept, so
// the replacement is never mistaken for ours and owned() reports the loss.
func (l *heldDirLock) touch(t time.Time) {
	if !l.owned() || touchLockPath(l.path, t, t) != nil {
		return
	}
	if info, err := os.Stat(l.path); err == nil && os.SameFile(l.info, info) {
		l.info = info
	}
}

// release removes the lock only while it is still ours.
func (l *heldDirLock) release() {
	if l.owned() {
		_ = os.Remove(l.path)
	}
}

// acquireDirLock takes a proper-lockfile lock: mkdir wins it, and an existing
// one is removed only once stale.
func acquireDirLock(path, takeover string, deadline time.Time) (*heldDirLock, error) {
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				return nil, err
			}
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			return &heldDirLock{path: path, info: info}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		st, statErr := os.Stat(path)
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			continue
		case statErr == nil && time.Since(st.ModTime()) > claudeLockStale:
			if err := removeStaleDirLock(path, st, takeover); err != nil {
				return nil, err
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, errClaudeRefreshing
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeStaleDirLock removes path only while it is still the stale directory
// seen, checked and removed under AIU's takeover lock. Without that, two
// contenders that both saw it stale could each remove it, and the second would
// delete the lock the first had just won. AIU processes cannot interleave here;
// Claude Code's proper-lockfile takes over stale locks unserialized, a window
// only its side can close.
func removeStaleDirLock(path string, seen fs.FileInfo, takeover string) error {
	return withFileLock(takeover, func() error {
		now, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !os.SameFile(seen, now) || time.Since(now.ModTime()) <= claudeLockStale {
			return nil // someone already took it over; contend for it again
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
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
