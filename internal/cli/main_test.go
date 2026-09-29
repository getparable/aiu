package cli

import (
	"os"
	"testing"

	"github.com/getparable/aiu/internal/core"
)

// No test may touch the developer's real Claude Code login: every read of it
// is a Keychain prompt, and every write replaces their session. Subprocess
// helpers inherit the override through os.Environ.
func TestMain(m *testing.M) {
	_ = os.Setenv("AIU_CLAUDE_SERVICE", "aiu-test-never-real")
	os.Exit(m.Run())
}

func TestDefaultConfigNeverTargetsRealClaudeLogin(t *testing.T) {
	if got := core.DefaultConfig().ClaudeService; got != "aiu-test-never-real" {
		t.Fatalf("tests would use Claude service %q", got)
	}
}
