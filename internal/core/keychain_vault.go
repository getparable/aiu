package core

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
)

// A single Keychain item means one access approval for AIU's account copies.
// Claude Code's own credential remains a separate item and is not migrated here.
const keychainVaultAccount = "aiu-accounts-v1"

type keychainVault struct {
	Version         int                `json:"version"`
	Records         map[string]*Record `json:"records"`
	MigratedConfigs map[string]bool    `json:"migratedConfigs,omitempty"`
	Aliases         map[string]string  `json:"aliases,omitempty"`
	DeletedRecords  map[string]bool    `json:"deletedRecords,omitempty"`
}

func (v *keychainVault) canonicalKey(key string) string {
	for hops := 0; hops < len(v.Aliases); hops++ {
		next, ok := v.Aliases[key]
		if !ok {
			break
		}
		key = next
	}
	return key
}

func (v *keychainVault) record(key string) *Record {
	key = v.canonicalKey(key)
	if v.DeletedRecords[key] {
		return nil
	}
	return v.Records[key]
}

// Tests supply an in-memory backend so they never touch the user's Keychain.
type keychainIO struct {
	lockPath string // Tests isolate the lock alongside their shared in-memory store.
	read     func(service, account string) (string, bool, error)
	write    func(service, account, secret string) error
	delete   func(service, account string) error
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

func (c *Config) keychainVaultLock() (string, error) {
	if c.keychainIO != nil && c.keychainIO.lockPath != "" {
		return c.keychainIO.lockPath, nil
	}
	// The Keychain item belongs to the user and service, not Config.Dir.
	// Native Keychain builds use cgo, so os/user reads the OS account database
	// instead of HOME, which callers can override independently of the Keychain.
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("cannot identify the Keychain owner: %w", err)
	}
	if !filepath.IsAbs(current.HomeDir) {
		return "", errors.New("Keychain owner has no absolute home directory")
	}
	identity := sha256.Sum256([]byte(c.StoreService + "\x00" + keychainVaultAccount))
	return filepath.Join(current.HomeDir, ".config", "aiu", "locks", fmt.Sprintf("keychain-%x.lock", identity)), nil
}

func (c *Config) keychainMigrationID() (string, error) {
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve AIU config directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("cannot resolve AIU config directory: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(dir))), nil
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
func (c *Config) migrateLegacyKeychain(vault *keychainVault) error {
	var idx Index
	if _, err := readJSONFile(c.indexFile(), &idx); err != nil {
		return fmt.Errorf("cannot migrate AIU Keychain items: %w", err)
	}
	for _, entry := range idx.Accounts {
		if entry == nil {
			return fmt.Errorf("cannot migrate AIU Keychain items: account index contains an empty entry")
		}
		key := storeKey(entry.Provider, entry.Email, entry.Org)
		// An existing vault record may have a newer rotated token than its backup.
		if vault.record(key) != nil || vault.DeletedRecords[vault.canonicalKey(key)] {
			continue
		}
		raw, ok, err := c.ownKeychainRead(key)
		if err != nil {
			return fmt.Errorf("cannot read old AIU Keychain item for %s: %w", key, err)
		}
		if !ok {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return fmt.Errorf("stored token for %s is corrupt: %w", key, err)
		}
		vault.Records[key] = &record
	}
	return nil
}

// Called with keychainVaultLock held. Persisting the complete migration before
// removing any old item makes a failed deletion or later write recoverable.
func (c *Config) loadOrMigrateKeychainVaultLocked() (*keychainVault, error) {
	migrationID, err := c.keychainMigrationID()
	if err != nil {
		return nil, err
	}
	vault, ok, err := c.readKeychainVault()
	if err != nil {
		return vault, err
	}
	if !ok {
		vault = &keychainVault{Version: 1, Records: make(map[string]*Record)}
	}
	if vault.MigratedConfigs[migrationID] {
		return vault, nil
	}
	if err := c.migrateLegacyKeychain(vault); err != nil {
		return nil, err
	}
	if vault.MigratedConfigs == nil {
		vault.MigratedConfigs = make(map[string]bool)
	}
	vault.MigratedConfigs[migrationID] = true
	if err := c.writeKeychainVault(vault); err != nil {
		return nil, err
	}
	return vault, nil
}

func (c *Config) loadKeychainVault() (*keychainVault, error) {
	migrationID, err := c.keychainMigrationID()
	if err != nil {
		return nil, err
	}
	vault, ok, err := c.readKeychainVault()
	if err != nil || (ok && vault.MigratedConfigs[migrationID]) {
		return vault, err
	}
	lockPath, err := c.keychainVaultLock()
	if err != nil {
		return nil, err
	}
	err = withFileLock(lockPath, func() error {
		vault, err = c.loadOrMigrateKeychainVaultLocked()
		return err
	})
	return vault, err
}

// Rekeying upgrades the index; it is not a request to remove legacy backups.
func (c *Config) rekeyKeychainToken(oldKey, newKey string, record *Record) error {
	return c.updateKeychainVault(func(vault *keychainVault) {
		if vault.record(newKey) == nil && !vault.DeletedRecords[newKey] {
			vault.Records[newKey] = record
		}
		if oldKey != newKey {
			if vault.Aliases == nil {
				vault.Aliases = make(map[string]string)
			}
			vault.Aliases[oldKey] = newKey
			delete(vault.Records, oldKey)
		}
	})
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
	lockPath, err := c.keychainVaultLock()
	if err != nil {
		return err
	}
	return withFileLock(lockPath, func() error {
		vault, err := c.loadOrMigrateKeychainVaultLocked()
		if err != nil {
			return err
		}
		change(vault)
		return c.writeKeychainVault(vault)
	})
}

func (c *Config) deleteKeychainToken(key string) error {
	lockPath, err := c.keychainVaultLock()
	if err != nil {
		return err
	}
	return withFileLock(lockPath, func() error {
		vault, err := c.loadOrMigrateKeychainVaultLocked()
		if err != nil {
			return err
		}
		key = vault.canonicalKey(key)
		if err := c.ownKeychainDelete(key); err != nil {
			return err
		}
		for alias := range vault.Aliases {
			if vault.canonicalKey(alias) == key {
				if err := c.ownKeychainDelete(alias); err != nil {
					return err
				}
				delete(vault.Records, alias)
			}
		}
		delete(vault.Records, key)
		if vault.DeletedRecords == nil {
			vault.DeletedRecords = make(map[string]bool)
		}
		vault.DeletedRecords[key] = true
		return c.writeKeychainVault(vault)
	})
}
