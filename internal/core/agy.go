package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	agyUsageSource  = "antigravity-statusline"
	agyStaleAfter   = 12 * time.Hour
	agyHookSubcmd   = "hook agy"
)

// AgyStatusLinePayload is the documented JSON payload Antigravity CLI pipes to a
// configured statusLine command on stdin (https://antigravity.google/docs/cli/statusline).
type AgyStatusLinePayload struct {
	Product  string                    `json:"product,omitempty"`
	Email    string                    `json:"email"`
	PlanTier string                    `json:"plan_tier,omitempty"`
	Quota    map[string]AgyQuotaBucket `json:"quota,omitempty"`
}

// AgyQuotaBucket is one entry in Antigravity CLI's statusLine quota map.
type AgyQuotaBucket struct {
	RemainingFraction *float64 `json:"remaining_fraction,omitempty"`
	RemainingAmount   *float64 `json:"remaining_amount,omitempty"`
	ResetTime         string   `json:"reset_time,omitempty"`
	ResetInSeconds    *float64 `json:"reset_in_seconds,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
}

type agyUsageEnvelope struct {
	Source    string                    `json:"source"`
	PlanTier  string                    `json:"plan_tier,omitempty"`
	FetchedAt string                    `json:"fetched_at,omitempty"`
	Quota     map[string]AgyQuotaBucket `json:"quota"`
}

// AgyHookStatus describes the state of AIU's statusLine hook in Antigravity CLI's
// settings.json.
type AgyHookStatus struct {
	Configured        bool   `json:"configured"`
	AlreadyConfigured bool   `json:"alreadyConfigured,omitempty"`
	CustomExisting    bool   `json:"customExisting,omitempty"`
	SettingsPath      string `json:"settingsPath"`
	Command           string `json:"command"`
	ExistingCommand   string `json:"existingCommand,omitempty"`
}

func (c *Config) indexLockFile() string {
	return filepath.Join(c.Dir, "accounts.lock")
}

// IngestAgyStatusLine records a quota snapshot piped by Antigravity CLI's
// statusLine hook. It stores no credentials, never accesses the Keychain, and
// makes no network requests. An unauthenticated payload (empty email, e.g.
// GEMINI_API_KEY mode) is a silent no-op returning (nil, nil).
func (c *Config) IngestAgyStatusLine(data []byte, label string) (*SavedAccount, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var payload AgyStatusLinePayload
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, fmt.Errorf("invalid Antigravity statusLine JSON: %w", err)
	}
	email := strings.TrimSpace(payload.Email)
	if email == "" {
		return nil, nil
	}
	planTier := strings.TrimSpace(payload.PlanTier)
	nowTime := c.now().UTC()
	nowMs := nowTime.UnixMilli()

	// Fast path: skip locking/rewriting accounts.json when the account is already
	// indexed with the same plan tier and no explicit label change was requested,
	// or when the user explicitly removed this email until they re-opt-in via `aiu add --agy`.
	saved, ignored, err := c.upsertPassiveAccount(Antigravity, email, label, planTier)
	if err != nil {
		return nil, err
	}
	if ignored {
		return nil, nil
	}

	key := storeKey(Antigravity, email, "")
	var usageRaw json.RawMessage
	if len(payload.Quota) > 0 {
		env := agyUsageEnvelope{
			Source:    agyUsageSource,
			PlanTier:  planTier,
			FetchedAt: nowTime.Format(time.RFC3339),
			Quota:     payload.Quota,
		}
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		usageRaw = raw
	}

	if err := c.withCacheLock(func(data cache) bool {
		changed := false
		act := data[agyActiveKey]
		if act == nil || act.Email != email || nowMs-act.VerifiedAt >= 15*1000 {
			data[agyActiveKey] = &cacheEntry{Email: email, VerifiedAt: nowMs}
			changed = true
		}
		if len(usageRaw) > 0 {
			e := data[key]
			if e == nil {
				e = &cacheEntry{}
			}
			// Always update when quota data changed or >15s passed since last write.
			if !bytes.Equal(e.Usage, usageRaw) || nowMs-e.FetchedAt >= 15*1000 || e.LastError != "" {
				e.Usage = usageRaw
				e.FetchedAt = nowMs
				e.AttemptedAt = nowMs
				e.LastError = ""
				data[key] = e
				changed = true
			}
		}
		return changed
	}); err != nil {
		// Cache contention during frequent statusLine renders should not fail the hook.
		if !errors.Is(err, errLocked) {
			return nil, err
		}
	}
	return saved, nil
}

func addIgnoredAgy(idx *Index, email string) {
	norm := strings.ToLower(strings.TrimSpace(email))
	if norm == "" {
		return
	}
	for _, existing := range idx.IgnoredAgy {
		if strings.ToLower(strings.TrimSpace(existing)) == norm {
			return
		}
	}
	idx.IgnoredAgy = append(idx.IgnoredAgy, norm)
}

func isIgnoredAgy(idx *Index, email string) bool {
	norm := strings.ToLower(strings.TrimSpace(email))
	if norm == "" {
		return false
	}
	for _, existing := range idx.IgnoredAgy {
		if strings.ToLower(strings.TrimSpace(existing)) == norm {
			return true
		}
	}
	return false
}

func (c *Config) clearIgnoredAgy() error {
	return withFileLock(c.indexLockFile(), func() error {
		idx, err := c.LoadIndex()
		if err != nil {
			return err
		}
		if len(idx.IgnoredAgy) == 0 {
			return nil
		}
		idx.IgnoredAgy = nil
		return c.saveIndex(idx)
	})
}

func (c *Config) upsertPassiveAccount(provider Provider, email, label, planTier string) (*SavedAccount, bool, error) {
	// Fast unlocked check to avoid rewriting accounts.json on every 300ms heartbeat.
	if idx, err := c.LoadIndex(); err == nil {
		if provider == Antigravity && label == "" && isIgnoredAgy(idx, email) {
			return nil, true, nil
		}
		for _, e := range idx.Accounts {
			if e.Provider == provider && e.Email == email && e.Org == "" {
				if (label == "" || e.Label == label) && (planTier == "" || e.PlanType == planTier) {
					return &SavedAccount{
						Record: &Record{
							Provider: provider,
							Email:    e.Email,
							Label:    e.Label,
							PlanType: e.PlanType,
							Source:   "statusline",
						},
						IsNew: false,
					}, false, nil
				}
				break
			}
		}
	}

	var saved *SavedAccount
	var ignored bool
	err := withFileLock(c.indexLockFile(), func() error {
		idx, err := c.LoadIndex()
		if err != nil {
			return err
		}
		if provider == Antigravity && label == "" && isIgnoredAgy(idx, email) {
			ignored = true
			return nil
		}
		var existing *IndexEntry
		for _, e := range idx.Accounts {
			if e.Provider == provider && e.Email == email && e.Org == "" {
				existing = e
				break
			}
		}
		effectiveLabel := label
		if effectiveLabel == "" && existing != nil {
			effectiveLabel = existing.Label
		}
		if effectiveLabel == "" {
			effectiveLabel = autoLabel(idx, provider, existing, email, "")
		}
		effectivePlan := firstNonEmpty(planTier, "")
		if existing != nil {
			if effectivePlan == "" {
				effectivePlan = existing.PlanType
			}
			existing.Label = effectiveLabel
			existing.PlanType = effectivePlan
		} else {
			idx.Accounts = append(idx.Accounts, &IndexEntry{
				Email:    email,
				Label:    effectiveLabel,
				Provider: provider,
				PlanType: effectivePlan,
				AddedAt:  c.now().UTC().Format(time.RFC3339),
			})
		}
		if err := c.saveIndex(idx); err != nil {
			return err
		}
		saved = &SavedAccount{
			Record: &Record{
				Provider: provider,
				Email:    email,
				Label:    effectiveLabel,
				PlanType: effectivePlan,
				Source:   "statusline",
			},
			IsNew: existing == nil,
		}
		return nil
	})
	return saved, ignored, err
}

// AgyActiveEmail returns the email address most recently seen from Antigravity CLI's
// statusLine hook, or empty when no reading has been recorded yet.
func (c *Config) AgyActiveEmail() string {
	if e := c.readCache()[agyActiveKey]; e != nil {
		return e.Email
	}
	return ""
}

// AgyHookCommand builds the statusLine command string for aiu.
func AgyHookCommand(aiuBin string) string {
	bin := strings.TrimSpace(aiuBin)
	if bin == "" {
		bin = "aiu"
	}
	return shellQuoteCommand(bin) + " " + agyHookSubcmd
}

// AgyHookSnippet returns the shell snippet to add inside an existing custom
// Antigravity statusLine script without replacing it.
func AgyHookSnippet(aiuBin string) string {
	cmd := AgyHookCommand(aiuBin)
	return "input=$(cat)\nprintf '%s' \"$input\" | " + cmd + " >/dev/null 2>&1 || true"
}

func shellQuoteCommand(s string) string {
	if s == "" {
		if runtime.GOOS == "windows" {
			return `""`
		}
		return "''"
	}
	safe := true
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '/' || r == '\\' || r == '.' || r == '_' || r == '-' || r == '~' || r == ':') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isAiuAgyHookCommand reports whether cmd is a standalone AIU Antigravity hook
// invocation (e.g. `~/.local/bin/aiu hook agy`), rejecting shell pipelines,
// chained commands, or custom wrapper scripts that merely call `aiu hook agy`.
func isAiuAgyHookCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" || strings.ContainsAny(trimmed, "|;&><$`\r\n()") {
		return false
	}
	args, ok := splitSimpleCommand(trimmed)
	if !ok || len(args) < 3 {
		return false
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(args[0], `\`, "/")))
	if base != "aiu" && base != "aiu.exe" {
		return false
	}
	if args[1] != "hook" {
		return false
	}
	rest := args[2:]
	if len(rest) == 1 && (rest[0] == "agy" || rest[0] == "antigravity" || rest[0] == "--agy" || rest[0] == "--antigravity") {
		return true
	}
	return false
}

func splitSimpleCommand(s string) ([]string, bool) {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inArg = true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, true
}

// AgyHookConfigured inspects Antigravity CLI's settings.json without modifying it.
func (c *Config) AgyHookConfigured() *AgyHookStatus {
	st := &AgyHookStatus{SettingsPath: c.AgySettingsFile}
	if c.AgySettingsFile == "" {
		return st
	}
	var doc map[string]json.RawMessage
	if ok, err := readJSONFile(c.AgySettingsFile, &doc); !ok || err != nil {
		return st
	}
	var sl map[string]any
	if json.Unmarshal(doc["statusLine"], &sl) != nil || sl == nil {
		return st
	}
	cmd := strings.TrimSpace(str(sl["command"]))
	if cmd == "" {
		return st
	}
	st.ExistingCommand = cmd
	if isAiuAgyHookCommand(cmd) {
		enabled := true
		if b, ok := sl["enabled"].(bool); ok {
			enabled = b
		}
		st.Configured = enabled
		st.Command = cmd
	} else {
		st.CustomExisting = true
	}
	return st
}

// SetupAgyStatusLine configures Antigravity CLI's official statusLine hook in
// ~/.gemini/antigravity-cli/settings.json with stack_with_default=true so the
// built-in status line stays intact. If the user already has a custom non-AIU
// statusLine command, it is left untouched and CustomExisting is reported.
func (c *Config) SetupAgyStatusLine(aiuBin string) (*AgyHookStatus, error) {
	if c.AgySettingsFile == "" {
		return nil, errors.New("Antigravity CLI settings path is not configured")
	}
	hookCmd := AgyHookCommand(aiuBin)
	doc := map[string]json.RawMessage{}
	data, err := os.ReadFile(c.AgySettingsFile)
	if err == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &doc); err != nil {
				return nil, fmt.Errorf("cannot parse %s: %w", c.AgySettingsFile, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	var sl map[string]any
	if raw, ok := doc["statusLine"]; ok && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &sl); err != nil {
			sl = nil
		}
	}
	if sl != nil {
		existing := strings.TrimSpace(str(sl["command"]))
		if existing != "" && !isAiuAgyHookCommand(existing) {
			return &AgyHookStatus{
				Configured:      false,
				CustomExisting:  true,
				SettingsPath:    c.AgySettingsFile,
				Command:         hookCmd,
				ExistingCommand: existing,
			}, nil
		}
		enabled := true
		if b, ok := sl["enabled"].(bool); ok {
			enabled = b
		}
		stacked, _ := sl["stack_with_default"].(bool)
		if existing == hookCmd && enabled && stacked {
			// Re-running `aiu add --agy` explicitly opts back in for previously removed accounts.
			if err := c.clearIgnoredAgy(); err != nil {
				return nil, err
			}
			return &AgyHookStatus{
				Configured:        true,
				AlreadyConfigured: true,
				SettingsPath:      c.AgySettingsFile,
				Command:           hookCmd,
				ExistingCommand:   existing,
			}, nil
		}
	} else {
		sl = map[string]any{}
	}

	sl["type"] = "command"
	sl["command"] = hookCmd
	sl["stack_with_default"] = true
	if b, ok := sl["enabled"].(bool); ok && !b {
		sl["enabled"] = true
	}
	slRaw, err := json.Marshal(sl)
	if err != nil {
		return nil, err
	}
	doc["statusLine"] = slRaw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomicFile(c.AgySettingsFile, append(out, '\n'), false, true); err != nil {
		return nil, err
	}
	if err := c.clearIgnoredAgy(); err != nil {
		return nil, err
	}
	return &AgyHookStatus{
		Configured:   true,
		SettingsPath: c.AgySettingsFile,
		Command:      hookCmd,
	}, nil
}

// agyHasRolledOver reports whether any quota bucket in raw had a reset_time in the past.
func agyHasRolledOver(raw json.RawMessage, now time.Time) bool {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	quota := obj(body["quota"])
	if len(quota) == 0 {
		return false
	}
	fetchedAt, _ := time.Parse(time.RFC3339Nano, str(body["fetched_at"]))
	for _, v := range quota {
		b := obj(v)
		if b == nil {
			continue
		}
		if _, hasFrac := num(b["remaining_fraction"]); !hasFrac {
			continue
		}
		if at, ok := agyBucketResetTime(b, fetchedAt); ok && !at.After(now) {
			return true
		}
	}
	return false
}

func agyBucketResetTime(b map[string]any, fetchedAt time.Time) (time.Time, bool) {
	if s := str(b["reset_time"]); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC(), true
		}
	}
	if secs, ok := num(b["reset_in_seconds"]); ok && secs > 0 && !fetchedAt.IsZero() {
		return fetchedAt.UTC().Add(time.Duration(secs * float64(time.Second))), true
	}
	return time.Time{}, false
}
