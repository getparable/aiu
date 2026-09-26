package core

import (
	"encoding/json"
	"errors"
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
}

func (m *memoryKeychain) io() *keychainIO {
	return &keychainIO{
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
	m := &memoryKeychain{items: make(map[string]string), readErr: make(map[string]error)}
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
