//go:build !darwin

package core

import "errors"

func readClaudeKeychain(service string) (string, string, bool, error) { return "", "", false, nil }

func writeClaudeKeychain(service, account, secret string) error {
	return errors.New("Keychain storage requires macOS")
}
