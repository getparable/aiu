//go:build !darwin

package core

import "errors"

func keychainWrite(service, account, secret string) error {
	return errors.New("Keychain storage requires macOS")
}

func keychainDelete(service, account string) error {
	return errors.New("Keychain storage requires macOS")
}

func keychainRead(service, account string) (string, bool, error) { return "", false, nil }

func readKeychainItem(service, account string) (string, string, bool, error) {
	return "", "", false, nil
}
