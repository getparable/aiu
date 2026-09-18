// Package core tracks rate-limit windows for several Claude (Pro/Max) and ChatGPT
// (Codex) accounts. It keeps its own copy of every account's OAuth tokens, reads
// usage from the endpoints the CLIs use for /usage and /status, and can point
// Claude Code or Codex at any tracked account.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Version is stamped by the build; the default marks a source build.
var Version = "dev"

// Provider names the subscription an account belongs to.
type Provider string

const (
	Claude Provider = "claude"
	Codex  Provider = "codex"
)

// Providers lists every provider in display order.
var Providers = []Provider{Claude, Codex}

// Name is the provider's product name.
func (p Provider) Name() string {
	if p == Codex {
		return "Codex"
	}
	return "Claude"
}

// Client is the CLI that holds the provider's login.
func (p Provider) Client() string {
	if p == Codex {
		return "Codex"
	}
	return "Claude Code"
}

// Glyph tells the providers apart without colour.
func (p Provider) Glyph() string {
	if p == Codex {
		return "⬢"
	}
	return "✳"
}

// ParseProvider maps user input to a provider; anything unrecognised is ok=false.
func ParseProvider(s string) (Provider, bool) {
	switch s {
	case "claude":
		return Claude, true
	case "codex":
		return Codex, true
	}
	return "", false
}

const (
	claudeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthBeta    = "oauth-2025-04-20"
	LoginScopesFull    = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	LoginScopesRead    = "user:profile"
	claudeCallbackPort = 54545
	callbackPath       = "/callback"
	loginTimeout       = 5 * time.Minute

	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexLoginScopes  = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexCallbackPath = "/auth/callback"
	codexOriginator   = "codex_cli_rs"
	// Used only when a Codex access token carries no exp claim.
	codexTokenLifetime = 8 * 24 * time.Hour
)

// codexCallbackPorts are the only loopback ports OpenAI registers for the Codex client.
var codexCallbackPorts = []int{1455, 1457}

// Request timing. The usage endpoints throttle per access token and, once tripped,
// stay tripped while you keep asking (multi-account-usage-bar measured 429s with two
// pollers ~90s apart, and six minutes of 10s polling never cleared). Every front end
// — CLI, watch, menu bar — shares one cache file under a lock, so these hold
// machine-wide.
const (
	MinFetchSpacing     = 5 * time.Minute
	limitedMemory       = time.Hour
	accountStagger      = 300 * time.Millisecond
	rateLimitCooldown   = 10 * time.Minute
	rateLimitCooldownMx = time.Hour
	profileRetrySpacing = 10 * time.Minute
	spacingTolerance    = 5 * time.Second
	inflightWait        = 20 * time.Second
	lockWait            = 15 * time.Second
	refreshMargin       = 5 * time.Minute
	LoginWarn           = 5 * 24 * time.Hour
)

// Config holds every path, endpoint and hook the core uses. DefaultConfig reads the
// environment; tests build one pointing at temp dirs and httptest servers.
type Config struct {
	Dir          string // index, cache, lock (and tokens.json with the file store)
	UseKeychain  bool
	StoreService string // keychain service for our own token copies

	ClaudeDir          string // Claude Code's config dir (.credentials.json lives here off macOS)
	ClaudeService      string // Claude Code's keychain service
	ClaudeGlobalConfig string // .claude.json with the cached oauthAccount block
	CodexHome          string

	ClaudeUsageURL            string
	ClaudeProfileURL          string
	ClaudeTokenURLs           []string
	ClaudeAuthorizeURL        string
	ClaudeAuthorizeConsoleURL string
	ClaudeManualRedirectURL   string
	CodexUsageURL             string
	CodexTokenURL             string
	CodexAuthorizeURL         string
	ReleaseAPIURL             string

	HTTP *http.Client
	Now  func() time.Time
	Warn func(string)
	Info func(string)
}

// DefaultConfig resolves paths the way Claude Code and Codex do.
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	c := &Config{
		Dir:          envOr("AIU_CONFIG_DIR", filepath.Join(home, ".config", "aiu")),
		UseKeychain:  runtime.GOOS == "darwin" && os.Getenv("AIU_STORE") != "file",
		StoreService: "aiu",

		CodexHome: envOr("CODEX_HOME", filepath.Join(home, ".codex")),

		ClaudeUsageURL:            "https://api.anthropic.com/api/oauth/usage",
		ClaudeProfileURL:          "https://api.anthropic.com/api/oauth/profile",
		ClaudeTokenURLs:           []string{"https://platform.claude.com/v1/oauth/token", "https://console.anthropic.com/v1/oauth/token"},
		ClaudeAuthorizeURL:        "https://claude.ai/oauth/authorize",
		ClaudeAuthorizeConsoleURL: "https://platform.claude.com/oauth/authorize",
		ClaudeManualRedirectURL:   "https://console.anthropic.com/oauth/code/callback",
		CodexUsageURL:             "https://chatgpt.com/backend-api/wham/usage",
		CodexTokenURL:             "https://auth.openai.com/oauth/token",
		CodexAuthorizeURL:         "https://auth.openai.com/oauth/authorize",
		ReleaseAPIURL:             releaseAPI,

		HTTP: &http.Client{Timeout: 20 * time.Second},
		Now:  time.Now,
		Warn: func(string) {},
		Info: func(string) {},
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		c.ClaudeDir = dir
		c.ClaudeGlobalConfig = filepath.Join(dir, ".claude.json")
		c.ClaudeService = ClaudeKeychainService(dir)
	} else {
		c.ClaudeDir = filepath.Join(home, ".claude")
		c.ClaudeGlobalConfig = filepath.Join(home, ".claude.json")
		c.ClaudeService = ClaudeKeychainService("")
	}
	if s := os.Getenv("AIU_CLAUDE_SERVICE"); s != "" {
		c.ClaudeService = s
	}
	return c
}

// ClaudeKeychainService mirrors Claude Code (verified in 2.1.273): the service gains
// "-" + sha256(dir)[:8] only when CLAUDE_CONFIG_DIR is set, hashed exactly as spelled.
func ClaudeKeychainService(configDir string) string {
	if configDir == "" {
		return "Claude Code-credentials"
	}
	sum := sha256.Sum256([]byte(norm.NFC.String(configDir)))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (c *Config) now() time.Time { return c.Now() }

func (c *Config) indexFile() string { return filepath.Join(c.Dir, "accounts.json") }
func (c *Config) fileStore() string { return filepath.Join(c.Dir, "tokens.json") }
func (c *Config) cacheFile() string { return filepath.Join(c.Dir, "usage-cache.json") }
func (c *Config) lockFile() string  { return filepath.Join(c.Dir, "usage-cache.lock") }
func (c *Config) codexAuthFile() string {
	return filepath.Join(c.CodexHome, "auth.json")
}

// StorageDescription says where tokens live, for humans.
func (c *Config) StorageDescription() string {
	if c.UseKeychain {
		return `macOS Keychain (service "` + c.StoreService + `")`
	}
	return c.fileStore()
}

// IndexPath is the account index location, for help text.
func (c *Config) IndexPath() string { return c.indexFile() }
