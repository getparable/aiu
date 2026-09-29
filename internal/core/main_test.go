package core

import (
	"os"
	"testing"
)

// No test may touch the developer's real Claude Code login: every read of it
// is a Keychain prompt, and every write replaces their session.
func TestMain(m *testing.M) {
	_ = os.Setenv("AIU_CLAUDE_SERVICE", "aiu-test-never-real")
	os.Exit(m.Run())
}
