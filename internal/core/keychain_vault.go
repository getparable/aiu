package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// A single Keychain item means one access approval for AIU's account copies.
// Claude Code's own credential remains a separate item and is not migrated here.
const keychainVaultAccount = "aiu-accounts-v1"

type keychainVault struct {
	Version int                `json:"version"`
	Records map[string]*Record `json:"records"`
}

// Tests supply an in-memory backend so they never touch the user's Keychain.
type keychainIO struct {
	read   func(service, account string) (string, bool, error)
	write  func(service, account, secret string) error
	delete func(service, account string) error
}

func (c *Config) ownKeychainRead(account string) (string, bool, error) {
	if c.keychainIO != nil {
		return c.keychainIO.read(c.StoreService, account)
	}
	return keychainRead(c.StoreService, account)
}

func (c *Config) ownKeychainWrite(account, secret string) error {
	if c.keychainIO != nil {
		return c.keychainIO.write(c.StoreService, account, secret)
	}
	return keychainWrite(c.StoreService, account, secret)
}

func (c *Config) ownKeychainDelete(account string) error {
	if c.keychainIO != nil {
		return c.keychainIO.delete(c.StoreService, account)
	}
	return keychainDelete(c.StoreService, account)
}

func (c *Config) keychainVaultLock() string {
	return filepath.Join(c.Dir, "keychain-vault.lock")
}

func (c *Config) readKeychainVault() (*keychainVault, bool, error) {
	raw, ok, err := c.ownKeychainRead(keychainVaultAccount)
	if err != nil || !ok {
		return nil, ok, err
	}
	var vault keychainVault
	if err := json.Unmarshal([]byte(raw), &vault); err != nil {
		return nil, true, fmt.Errorf("AIU Keychain vault is unreadable: %w", err)
	}
	if vault.Version != 1 || vault.Records == nil {
		return nil, true, fmt.Errorf("AIU Keychain vault has an unsupported format (version %d)", vault.Version)
	}
	return &vault, true, nil
}

func (c *Config) writeKeychainVault(vault *keychainVault) error {
	raw, err := json.Marshal(vault)
	if err != nil {
		return err
	}
	return c.ownKeychainWrite(keychainVaultAccount, string(raw))
}

// migrateLegacyKeychain reads each tracked account before writing anything. An
// interrupted or denied read leaves every old item intact, so retry is safe.
// Old items remain untouched at migration time. They can become stale after
// token refreshes; once the vault exists, AIU reads only it.
func (c *Config) migrateLegacyKeychain() (*keychainVault, error) {
	var idx Index
	if _, err := readJSONFile(c.indexFile(), &idx); err != nil {
		return nil, fmt.Errorf("cannot migrate AIU Keychain items: %w", err)
	}
	vault := &keychainVault{Version: 1, Records: make(map[string]*Record)}
	for _, entry := range idx.Accounts {
		if entry == nil {
			return nil, fmt.Errorf("cannot migrate AIU Keychain items: account index contains an empty entry")
		}
		key := storeKey(entry.Provider, entry.Email, entry.Org)
		raw, ok, err := c.ownKeychainRead(key)
		if err != nil {
			return nil, fmt.Errorf("cannot read old AIU Keychain item for %s: %w", key, err)
		}
		if !ok {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, fmt.Errorf("stored token for %s is corrupt: %w", key, err)
		}
		vault.Records[key] = &record
	}
	return vault, nil
}

// Called with keychainVaultLock held. Persisting the complete migration before
// removing any old item makes a failed deletion or later write recoverable.
func (c *Config) loadOrMigrateKeychainVaultLocked() (*keychainVault, error) {
	vault, ok, err := c.readKeychainVault()
	if err != nil || ok {
		return vault, err
	}
	vault, err = c.migrateLegacyKeychain()
	if err != nil {
		return nil, err
	}
	if err := c.writeKeychainVault(vault); err != nil {
		return nil, err
	}
	return vault, nil
}

func (c *Config) loadKeychainVault() (*keychainVault, error) {
	vault, ok, err := c.readKeychainVault()
	if err != nil || ok {
		return vault, err
	}
	err = withFileLock(c.keychainVaultLock(), func() error {
		vault, err = c.loadOrMigrateKeychainVaultLocked()
		return err
	})
	return vault, err
}

// MigrateKeychainVault copies legacy AIU items into one Keychain item. It does
// not read Claude Code's login or contact either provider.
func (c *Config) MigrateKeychainVault() (int, error) {
	if !c.UseKeychain {
		return 0, errors.New("AIU is not using the macOS Keychain")
	}
	vault, err := c.loadKeychainVault()
	if err != nil {
		return 0, err
	}
	return len(vault.Records), nil
}

func (c *Config) updateKeychainVault(change func(*keychainVault)) error {
	return withFileLock(c.keychainVaultLock(), func() error {
		vault, err := c.loadOrMigrateKeychainVaultLocked()
		if err != nil {
			return err
		}
		change(vault)
		return c.writeKeychainVault(vault)
	})
}

func (c *Config) deleteKeychainToken(key string) error {
	return withFileLock(c.keychainVaultLock(), func() error {
		vault, err := c.loadOrMigrateKeychainVaultLocked()
		if err != nil {
			return err
		}
		if err := c.ownKeychainDelete(key); err != nil {
			return err
		}
		delete(vault.Records, key)
		return c.writeKeychainVault(vault)
	})
}
