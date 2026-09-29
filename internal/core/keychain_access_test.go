package core

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The status check must predict every prompt loadKeychainVault can cause: the
// vault itself and, until this configuration has migrated, each old per-account
// item migration still has to copy in.
func TestVaultKeychainAccessCoversPendingLegacyItems(t *testing.T) {
	a := &IndexEntry{Provider: Claude, Email: "a@example.test", Org: "org-a"}
	b := &IndexEntry{Provider: Codex, Email: "b@example.test", Org: "acct-b"}
	keyA, keyB := storeKey(a.Provider, a.Email, a.Org), storeKey(b.Provider, b.Email, b.Org)
	for _, tc := range []struct {
		name   string
		index  []*IndexEntry
		vault  func(migrationID string) *keychainVault // nil: no vault item
		asking []string                                // items macOS would ask about
		want   string
		probed []string // legacy items the check may read
	}{
		{name: "no vault, nothing tracked", want: AccessMissing},
		{name: "no vault, a legacy item would prompt", index: []*IndexEntry{a, b}, asking: []string{keyB},
			want: AccessNeedsApproval, probed: []string{keyA, keyB}},
		{name: "no vault, legacy items readable", index: []*IndexEntry{a, b}, want: AccessGranted, probed: []string{keyA, keyB}},
		{name: "migrated vault never probes legacy items", index: []*IndexEntry{a, b}, asking: []string{keyA, keyB},
			vault: func(id string) *keychainVault {
				return &keychainVault{Version: 1, Records: map[string]*Record{}, MigratedConfigs: map[string]bool{id: true}}
			}, want: AccessGranted},
		{name: "unmigrated vault probes only accounts it lacks", index: []*IndexEntry{a, b}, asking: []string{keyA, keyB},
			vault: func(string) *keychainVault {
				return &keychainVault{Version: 1, Records: map[string]*Record{keyA: {Provider: Claude, Email: a.Email}}}
			}, want: AccessNeedsApproval, probed: []string{keyB}},
		{name: "vault itself would prompt", index: []*IndexEntry{a}, asking: []string{keychainVaultAccount}, want: AccessNeedsApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t, newFakeAPI(t))
			c.UseKeychain = true
			if err := os.MkdirAll(c.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.index != nil {
				b, err := json.Marshal(&Index{Version: 1, Accounts: tc.index})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(c.indexFile(), b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			items := map[string]string{keyA: "{}", keyB: "{}"}
			if tc.vault != nil {
				id, err := c.keychainMigrationID()
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(tc.vault(id))
				if err != nil {
					t.Fatal(err)
				}
				items[keychainVaultAccount] = string(raw)
			}
			var probed []string
			previous := silentKeychainRead
			silentKeychainRead = func(service, account string) (string, string, string) {
				if account != keychainVaultAccount {
					probed = append(probed, account)
				}
				if slices.Contains(tc.asking, account) {
					return "", AccessNeedsApproval, "would ask"
				}
				if raw, ok := items[account]; ok {
					return raw, AccessGranted, ""
				}
				return "", AccessMissing, ""
			}
			t.Cleanup(func() { silentKeychainRead = previous })

			if got := c.vaultKeychainAccess(); got.State != tc.want {
				t.Fatalf("state %s (%s), want %s", got.State, got.Detail, tc.want)
			}
			if !slices.Equal(probed, tc.probed) {
				t.Fatalf("probed %v, want %v", probed, tc.probed)
			}
		})
	}
}
