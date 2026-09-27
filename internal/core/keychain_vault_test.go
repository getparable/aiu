package core

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

type memoryKeychain struct {
	mu          sync.Mutex
	items       map[string]string
	reads       []string
	readErr     map[string]error
	writeErr    error
	writes      int
	failWriteAt int
	lockPath    string
}

func (m *memoryKeychain) io() *keychainIO {
	return &keychainIO{
		lockPath: m.lockPath,
		read: func(_, account string) (string, bool, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.reads = append(m.reads, account)
			if err := m.readErr[account]; err != nil {
				return "", false, err
			}
			value, ok := m.items[account]
			return value, ok, nil
		},
		write: func(_, account, secret string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.writes++
			if m.writeErr != nil || m.writes == m.failWriteAt {
				if m.writeErr == nil {
					return errors.New("write failed")
				}
				return m.writeErr
			}
			m.items[account] = secret
			return nil
		},
		delete: func(_, account string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.items, account)
			return nil
		},
	}
}

func vaultTestConfig(t *testing.T, entries ...*IndexEntry) (*Config, *memoryKeychain) {
	t.Helper()
	m := &memoryKeychain{items: make(map[string]string), readErr: make(map[string]error), lockPath: filepath.Join(t.TempDir(), "vault.lock")}
	c := &Config{Dir: t.TempDir(), UseKeychain: true, StoreService: "aiu-test", keychainIO: m.io()}
	if err := c.saveIndex(&Index{Version: 1, Accounts: entries}); err != nil {
		t.Fatal(err)
	}
	return c, m
}

func legacyRecord(t *testing.T, m *memoryKeychain, key, token string) {
	t.Helper()
	raw, err := json.Marshal(&Record{AccessToken: token})
	if err != nil {
		t.Fatal(err)
	}
	m.items[key] = string(raw)
}

func TestKeychainVaultMigratesOnceAndReadsOneItem(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test"}
	b := &IndexEntry{Provider: Codex, Email: "b@example.test"}
	c, m := vaultTestConfig(t, a, b)
	legacyRecord(t, m, storeKey(a.Provider, a.Email, ""), "a-token")
	legacyRecord(t, m, storeKey(b.Provider, b.Email, ""), "b-token")

	idx, err := c.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	records, err := c.LoadRecords(idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].AccessToken != "a-token" || records[1].AccessToken != "b-token" {
		t.Fatalf("migration lost an account: %#v", records)
	}
	if _, ok := m.items[keychainVaultAccount]; !ok {
		t.Fatal("migration did not create the vault")
	}
	if _, ok := m.items[storeKey(a.Provider, a.Email, "")]; !ok {
		t.Fatal("migration removed the old item")
	}

	m.reads = nil
	records, err = c.LoadRecords(idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || len(m.reads) != 1 || m.reads[0] != keychainVaultAccount {
		t.Fatalf("later load should read one Keychain item, got reads %v", m.reads)
	}

	if err := c.tokenDelete(storeKey(b.Provider, b.Email, "")); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.items[storeKey(b.Provider, b.Email, "")]; ok {
		t.Fatal("remove kept the old account item")
	}
	if r, err := c.tokenGet(storeKey(b.Provider, b.Email, "")); err != nil || r != nil {
		t.Fatalf("removed account returned %v, %v", r, err)
	}
}

func TestKeychainVaultMigrationFailurePreservesOldItems(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test"}
	b := &IndexEntry{Provider: Codex, Email: "b@example.test"}
	c, m := vaultTestConfig(t, a, b)
	first := storeKey(a.Provider, a.Email, "")
	second := storeKey(b.Provider, b.Email, "")
	legacyRecord(t, m, first, "a-token")
	legacyRecord(t, m, second, "b-token")
	m.readErr[second] = errors.New("access denied")
	if _, err := c.tokenGet(first); err == nil {
		t.Fatal("denied legacy read should stop migration")
	}
	if _, ok := m.items[keychainVaultAccount]; ok {
		t.Fatal("partial migration wrote the vault")
	}
	if len(m.items) != 2 {
		t.Fatalf("partial migration changed old items: %d", len(m.items))
	}
	delete(m.readErr, second)
	m.writeErr = errors.New("vault write denied")
	if _, err := c.tokenGet(first); err == nil {
		t.Fatal("failed vault write should stop migration")
	}
	if _, ok := m.items[keychainVaultAccount]; ok {
		t.Fatal("failed write created a partial vault")
	}
	if len(m.items) != 2 {
		t.Fatalf("failed write changed old items: %d", len(m.items))
	}
	m.writeErr = nil
	if r, err := c.tokenGet(second); err != nil || r.AccessToken != "b-token" {
		t.Fatalf("retry did not recover: %v, %v", r, err)
	}
}

func TestKeychainVaultDeleteKeepsARecoverableCopyOnWriteFailure(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test"}
	c, m := vaultTestConfig(t, a)
	key := storeKey(a.Provider, a.Email, "")
	legacyRecord(t, m, key, "a-token")
	m.failWriteAt = 2
	if err := c.tokenDelete(key); err == nil {
		t.Fatal("the failed vault update should be reported")
	}
	r, err := c.tokenGet(key)
	if err != nil || r == nil || r.AccessToken != "a-token" {
		t.Fatalf("failed delete lost the token: %v, %v", r, err)
	}
}

func TestKeychainVaultConcurrentUpdatesKeepBothAccounts(t *testing.T) {
	c, _ := vaultTestConfig(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"claude:a@example.test", "codex:b@example.test"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.tokenSet(key, &Record{AccessToken: key})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"claude:a@example.test", "codex:b@example.test"} {
		r, err := c.tokenGet(key)
		if err != nil || r == nil || r.AccessToken != key {
			t.Fatalf("concurrent update lost %s: %v, %v", key, r, err)
		}
	}
}

func TestKeychainVaultMigratesEveryConfigWithoutReplacingFreshTokens(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test"}
	b := &IndexEntry{Provider: Codex, Email: "b@example.test"}
	c, m := vaultTestConfig(t, a)
	other := *c
	other.Dir = t.TempDir()
	if err := other.saveIndex(&Index{Version: 1, Accounts: []*IndexEntry{a, b}}); err != nil {
		t.Fatal(err)
	}
	aKey, bKey := storeKey(a.Provider, a.Email, ""), storeKey(b.Provider, b.Email, "")
	legacyRecord(t, m, aKey, "old-a")
	legacyRecord(t, m, bKey, "old-b")
	if _, err := c.MigrateKeychainVault(); err != nil {
		t.Fatal(err)
	}
	if err := c.tokenSet(aKey, &Record{AccessToken: "fresh-a"}); err != nil {
		t.Fatal(err)
	}
	if count, err := other.MigrateKeychainVault(); err != nil || count != 2 {
		t.Fatalf("second config did not migrate its account: count=%d err=%v", count, err)
	}
	for key, want := range map[string]string{aKey: "fresh-a", bKey: "old-b"} {
		r, err := other.tokenGet(key)
		if err != nil || r == nil || r.AccessToken != want {
			t.Fatalf("incorrect merged record for %s: %#v, %v", key, r, err)
		}
	}
	m.reads = nil
	if _, err := other.MigrateKeychainVault(); err != nil {
		t.Fatal(err)
	}
	if len(m.reads) != 1 || m.reads[0] != keychainVaultAccount {
		t.Fatalf("completed config migration reread legacy credentials: %v", m.reads)
	}
}

func TestKeychainVaultLockScopeMatchesSharedStore(t *testing.T) {
	c, _ := vaultTestConfig(t)
	other := *c
	other.Dir = t.TempDir()
	if vaultLockPath(t, c) != vaultLockPath(t, &other) {
		t.Fatal("configs sharing one vault use different locks")
	}
	// Check production scope too, without opening a real lock or Keychain item.
	c.keychainIO, other.keychainIO = nil, nil
	if vaultLockPath(t, c) != vaultLockPath(t, &other) {
		t.Fatal("production configs sharing one vault use different locks")
	}
	other.StoreService = "different-service"
	if vaultLockPath(t, c) == vaultLockPath(t, &other) {
		t.Fatal("different vault services should have independent locks")
	}
}

func vaultLockPath(t *testing.T, c *Config) string {
	t.Helper()
	path, err := c.keychainVaultLock()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKeychainVaultLockIgnoresHOMEOverride(t *testing.T) {
	c := &Config{Dir: t.TempDir(), StoreService: "aiu-test"}
	first := vaultLockPath(t, c)
	t.Setenv("HOME", t.TempDir())
	if second := vaultLockPath(t, c); second != first {
		t.Fatal("HOME override changed the lock for the same Keychain owner")
	}
}

func TestKeychainVaultOrganizationRekeyPreservesLegacyBackup(t *testing.T) {
	a := &IndexEntry{Provider: Codex, Email: "a@example.test"}
	c, m := vaultTestConfig(t, a)
	oldKey := storeKey(a.Provider, a.Email, "")
	raw, err := json.Marshal(&Record{AccessToken: "old-token", AccountID: "organization"})
	if err != nil {
		t.Fatal(err)
	}
	m.items[oldKey] = string(raw)
	c.Warn = func(message string) { t.Error(message) }
	idx, err := c.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Accounts[0].Org != "organization" {
		t.Fatal("organization was not added to index")
	}
	if m.items[oldKey] != string(raw) {
		t.Fatal("automatic rekey removed or changed the legacy backup")
	}
	newKey := storeKey(a.Provider, a.Email, "organization")
	if r, err := c.tokenGet(newKey); err != nil || r == nil || r.AccessToken != "old-token" {
		t.Fatalf("rekey lost the account: %#v, %v", r, err)
	}
	vault, err := c.loadKeychainVault()
	if err != nil || vault.Records[oldKey] != nil || vault.Aliases[oldKey] != newKey {
		t.Fatalf("old key should be an alias to the canonical record: %#v, %v", vault, err)
	}
}

func TestKeychainVaultOldIndexUsesFreshCanonicalCredentials(t *testing.T) {
	a := &IndexEntry{Provider: Codex, Email: "a@example.test"}
	c, m := vaultTestConfig(t, a)
	other := *c
	other.Dir = t.TempDir()
	if err := other.saveIndex(&Index{Version: 1, Accounts: []*IndexEntry{a}}); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := storeKey(a.Provider, a.Email, ""), storeKey(a.Provider, a.Email, "organization")
	raw, err := json.Marshal(&Record{AccessToken: "stale", AccountID: "organization"})
	if err != nil {
		t.Fatal(err)
	}
	m.items[oldKey] = string(raw)
	c.Warn = func(message string) { t.Error(message) }
	other.Warn = c.Warn
	if _, err := c.LoadIndex(); err != nil {
		t.Fatal(err)
	}
	if err := c.tokenSet(newKey, &Record{AccessToken: "fresh", AccountID: "organization"}); err != nil {
		t.Fatal(err)
	}
	idx, err := other.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	records, err := other.LoadRecords(idx)
	if err != nil || len(records) != 1 || records[0].AccessToken != "fresh" || idx.Accounts[0].Org != "organization" {
		t.Fatalf("old index did not use fresh canonical credentials: %#v, %v", records, err)
	}
	if err := c.tokenDelete(newKey); err != nil {
		t.Fatal(err)
	}
	// A third config still has the old index. It must not restore the removed account.
	third := other
	third.Dir = t.TempDir()
	if err := third.saveIndex(&Index{Version: 1, Accounts: []*IndexEntry{a}}); err != nil {
		t.Fatal(err)
	}
	idx, err = third.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	records, err = third.LoadRecords(idx)
	if err != nil || len(records) != 1 || !records[0].Missing {
		t.Fatalf("old index restored a removed account: %#v, %v", records, err)
	}
	if _, exists := m.items[oldKey]; exists {
		t.Fatal("explicit remove retained the legacy alias credential")
	}
	// A new login can intentionally add the account again.
	if err := c.tokenSet(newKey, &Record{AccessToken: "new-login", AccountID: "organization"}); err != nil {
		t.Fatal(err)
	}
	if r, err := third.tokenGet(oldKey); err != nil || r == nil || r.AccessToken != "new-login" {
		t.Fatalf("intentional new login did not restore the account: %#v, %v", r, err)
	}
}

func TestKeychainVaultConcurrentConfigsKeepEveryUpdate(t *testing.T) {
	c, _ := vaultTestConfig(t)
	const count = 12
	configs := make([]Config, count)
	for i := range configs {
		configs[i] = *c
		configs[i].Dir = t.TempDir()
		if err := configs[i].saveIndex(&Index{Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range configs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := configs[i].Dir
			errs <- configs[i].tokenSet(key, &Record{AccessToken: key})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range configs {
		key := configs[i].Dir
		r, err := configs[i].tokenGet(key)
		if err != nil || r == nil || r.AccessToken != key {
			t.Fatalf("concurrent config update was lost: %#v, %v", r, err)
		}
	}
}

func TestKeychainVaultExistingFormatMigrationRetriesAtomically(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test"}
	b := &IndexEntry{Provider: Codex, Email: "b@example.test"}
	c, m := vaultTestConfig(t, a, b)
	aKey, bKey := storeKey(a.Provider, a.Email, ""), storeKey(b.Provider, b.Email, "")
	legacyRecord(t, m, aKey, "a-token")
	legacyRecord(t, m, bKey, "b-token")
	// A deployed v0.3.1 vault has no per-configuration migration markers.
	m.items[keychainVaultAccount] = `{"version":1,"records":{}}`
	original := m.items[keychainVaultAccount]
	m.readErr[bKey] = errors.New("access denied")
	if _, err := c.MigrateKeychainVault(); err == nil {
		t.Fatal("denied read should prevent partial migration")
	}
	if m.items[keychainVaultAccount] != original {
		t.Fatal("failed migration modified the existing vault")
	}
	delete(m.readErr, bKey)
	m.writeErr = errors.New("write denied")
	if _, err := c.MigrateKeychainVault(); err == nil {
		t.Fatal("denied write should leave migration incomplete")
	}
	if m.items[keychainVaultAccount] != original {
		t.Fatal("denied write modified the existing vault")
	}
	m.writeErr = nil
	if count, err := c.MigrateKeychainVault(); err != nil || count != 2 {
		t.Fatalf("retry did not recover both accounts: %d, %v", count, err)
	}
}
