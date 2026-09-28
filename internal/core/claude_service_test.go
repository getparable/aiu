package core

import (
	"os"
	"path/filepath"
	"testing"
)

// Mirrors Claude Code 2.1.284: CLAUDE_SECURESTORAGE_CONFIG_DIR, when set at all,
// decides both the Keychain service and where .credentials.json lives.
func TestDefaultConfigClaudeSecureStorage(t *testing.T) {
	home, _ := os.UserHomeDir()
	const work = "/Users/example/.claude-accounts/work"
	const other = "/Users/example/.claude-accounts/other"
	for _, tc := range []struct {
		name          string
		configDir     string
		storage       *string
		service, dir  string
		globalConfig  string
		aiuServiceEnv string
	}{
		{name: "defaults", service: "Claude Code-credentials", dir: filepath.Join(home, ".claude"), globalConfig: filepath.Join(home, ".claude.json")},
		{name: "config dir", configDir: work, service: "Claude Code-credentials-ce139327", dir: work, globalConfig: filepath.Join(work, ".claude.json")},
		{name: "storage dir", storage: ptr(work), service: "Claude Code-credentials-ce139327", dir: work, globalConfig: filepath.Join(home, ".claude.json")},
		{name: "storage overrides config dir", configDir: other, storage: ptr(work), service: "Claude Code-credentials-ce139327", dir: work, globalConfig: filepath.Join(other, ".claude.json")},
		{name: "empty storage means default login", configDir: work, storage: ptr(""), service: "Claude Code-credentials", dir: filepath.Join(home, ".claude"), globalConfig: filepath.Join(work, ".claude.json")},
		{name: "AIU_CLAUDE_SERVICE wins", storage: ptr(work), aiuServiceEnv: "aiu-explicit", service: "aiu-explicit", dir: work, globalConfig: filepath.Join(home, ".claude.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tc.configDir)
			t.Setenv("AIU_CLAUDE_SERVICE", tc.aiuServiceEnv)
			t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
			if tc.storage == nil {
				_ = os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
			} else {
				t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", *tc.storage)
			}
			c := DefaultConfig()
			if c.ClaudeService != tc.service || c.ClaudeDir != tc.dir || c.ClaudeGlobalConfig != tc.globalConfig {
				t.Fatalf("service=%q dir=%q global=%q", c.ClaudeService, c.ClaudeDir, c.ClaudeGlobalConfig)
			}
		})
	}
}

func ptr(s string) *string { return &s }
