//go:build !darwin || !cgo

package core

import "errors"

// Without cgo there is no way to inspect an item's access control short of
// reading it, which could prompt.
func claudeItemTrustsSecurity(service string) (found, trusted bool, err error) {
	return true, false, errors.New("this build cannot check Keychain access without asking")
}

func silentItemAccess(service, account string) (string, string) {
	return AccessUnknown, "this build cannot check Keychain access without asking"
}
