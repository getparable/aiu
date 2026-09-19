//go:build darwin && !cgo

package core

import "errors"

var errNoCGO = errors.New("Keychain storage requires a build with CGO_ENABLED=1")

func keychainWrite(service, account, secret string) error        { return errNoCGO }
func keychainDelete(service, account string) error               { return errNoCGO }
func keychainRead(service, account string) (string, bool, error) { return "", false, errNoCGO }
func readKeychainItem(service, account string) (string, string, bool, error) {
	return "", "", false, errNoCGO
}
