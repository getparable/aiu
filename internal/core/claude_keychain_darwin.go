//go:build darwin

package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
	"time"
)

const securityBin = "/usr/bin/security"

// Long enough for the user to answer a Keychain prompt, short enough that a
// forgotten one does not hang the menu bar forever.
const securityTimeout = 2 * time.Minute

// errSecItemNotFound, as `security` reports it in its exit status.
const securityExitNotFound = 44

// runSecurity never logs its arguments or output: a write carries the secret.
func runSecurity(args ...string) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), securityTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, securityBin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", "", -1, errors.New("timed out waiting for the Keychain")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.String(), stderr.String(), exit.ExitCode(), err
	}
	return stdout.String(), stderr.String(), 0, err
}

func securityError(op, stderr string, err error) error {
	if msg := strings.TrimSpace(Redact(stderr)); msg != "" {
		return errors.New("Claude Code Keychain " + op + " failed: " + msg)
	}
	return errors.New("Claude Code Keychain " + op + " failed: " + err.Error())
}

// readClaudeKeychain returns Claude Code's login and the account it is stored
// under. Claude Code reads only the item filed under currentUser(), so that one
// wins when several share the service; the secret read is scoped to whichever
// item was found.
func readClaudeKeychain(service string) (string, string, bool, error) {
	if service == "" {
		return "", "", false, nil
	}
	attrs, stderr, code, err := runSecurity("find-generic-password", "-a", currentUser(), "-s", service)
	if code == securityExitNotFound {
		attrs, stderr, code, err = runSecurity("find-generic-password", "-s", service)
	}
	if code == securityExitNotFound {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, securityError("lookup", stderr, err)
	}
	args := []string{"find-generic-password", "-s", service, "-w"}
	account, _ := keychainBlobAttr(attrs, "acct")
	if account != "" {
		args = append(args, "-a", account)
	}
	out, stderr, code, err := runSecurity(args...)
	if code == securityExitNotFound {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, securityError("read", stderr, err)
	}
	return decodeSecurityPassword(out), account, true, nil
}

// writeClaudeKeychain updates the item in place, keeping its access control.
// The hex secret is briefly visible in argv, which macOS shows only to this
// user's processes and root; Claude Code writes large logins the same way.
func writeClaudeKeychain(service, account, secret string) error {
	_, stderr, _, err := runSecurity("add-generic-password", "-U",
		"-a", account, "-s", service, "-X", hex.EncodeToString([]byte(secret)))
	if err != nil {
		return securityError("write", stderr, err)
	}
	return nil
}
