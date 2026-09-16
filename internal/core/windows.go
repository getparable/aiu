package core

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Window is one rate-limit window, normalised across providers and body shapes.
type Window struct {
	Key      string  `json:"key"`
	Group    string  `json:"group"` // "session" (≤24h) or "weekly"
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	Known    bool    `json:"-"` // false when the body carried no percentage
	ResetsAt string  `json:"resetsAt,omitempty"`
	Severity string  `json:"severity,omitempty"` // warning, critical or locked
}

// ResetTime parses ResetsAt; ok=false when there is none.
func (w Window) ResetTime() (time.Time, bool) {
	if w.ResetsAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
	return t, err == nil
}

// normalizeTime rewrites an API timestamp (Anthropic sends microseconds) as plain
// RFC 3339 UTC, which every consumer of --json can parse.
func normalizeTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

var windowLabels = map[string]string{
	"five_hour":            "5h session",
	"seven_day":            "7d all",
	"seven_day_opus":       "7d Opus",
	"seven_day_sonnet":     "7d Sonnet",
	"seven_day_oauth_apps": "7d OAuth apps",
}

var windowOrder = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps"}

// Severity thresholds, in percent used.
const (
	WarnPercent     = 60
	CriticalPercent = 80
)

// SeverityFor classifies a percentage: ok, warning or critical.
func SeverityFor(p float64) string {
	switch {
	case p >= CriticalPercent:
		return "critical"
	case p >= WarnPercent:
		return "warning"
	}
	return "ok"
}

func clampPct(v any) (float64, bool) {
	n, ok := num(v)
	if !ok || math.IsNaN(n) {
		return 0, false
	}
	return math.Max(0, math.Min(100, n)), true
}

// NormalizeWindows turns a raw usage body into windows, most important first.
func NormalizeWindows(raw json.RawMessage, now time.Time) []Window {
	if len(raw) == 0 {
		return nil
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	if _, hasLimits := body["limits"].([]any); !hasLimits {
		if _, ok := body["rate_limit"]; ok {
			return codexWindows(body, now)
		}
		if _, ok := body["plan_type"]; ok {
			return codexWindows(body, now)
		}
	}
	if limits, ok := body["limits"].([]any); ok && len(limits) > 0 {
		return limitWindows(limits)
	}
	return claudeWindows(body)
}

func claudeWindows(body map[string]any) []Window {
	keys := make([]string, 0, len(body))
	for k, v := range body {
		m := obj(v)
		if k == "extra_usage" || k == "spend" || m == nil {
			continue
		}
		if _, ok := m["utilization"]; !ok {
			continue
		}
		p, _ := clampPct(m["utilization"])
		// Unknown codenamed windows appear only once they carry something.
		if windowLabels[k] == "" && p <= 0 && str(m["resets_at"]) == "" {
			continue
		}
		keys = append(keys, k)
	}
	rank := func(k string) int {
		for i, o := range windowOrder {
			if o == k {
				return i
			}
		}
		return 99
	}
	sort.Slice(keys, func(i, j int) bool {
		if rank(keys[i]) != rank(keys[j]) {
			return rank(keys[i]) < rank(keys[j])
		}
		return keys[i] < keys[j]
	})
	out := make([]Window, 0, len(keys))
	for _, k := range keys {
		m := obj(body[k])
		p, known := clampPct(m["utilization"])
		w := Window{Key: k, Group: "weekly", Label: firstNonEmpty(windowLabels[k], k), Percent: p, Known: known, ResetsAt: normalizeTime(str(m["resets_at"]))}
		if k == "five_hour" {
			w.Group = "session"
		}
		if str(m["locked_reason"]) != "" {
			w.Severity = "locked"
		} else if known {
			w.Severity = severityTag(p)
		}
		out = append(out, w)
	}
	return out
}

func limitWindows(limits []any) []Window {
	out := make([]Window, 0, len(limits))
	for _, l := range limits {
		m := obj(l)
		kind := str(m["kind"])
		scope := obj(m["scope"])
		scopeName := firstNonEmpty(str(obj(scope["model"])["display_name"]), str(obj(scope["model"])["id"]), str(scope["surface"]))
		group := str(m["group"])
		if group == "" {
			group = "weekly"
			if kind == "session" {
				group = "session"
			}
		}
		label := kind
		switch {
		case kind == "session":
			label = "5h session"
		case kind == "weekly_all":
			label = "7d all"
		case group == "weekly":
			label = "7d " + firstNonEmpty(scopeName, strings.TrimPrefix(kind, "weekly_"))
		case scopeName != "":
			label = kind + " " + scopeName
		}
		key := kind
		if scope != nil {
			key += ":" + firstNonEmpty(scopeName, "scoped")
		}
		p, known := clampPct(m["percent"])
		sev := str(m["severity"])
		if sev == "normal" {
			sev = ""
		}
		out = append(out, Window{Key: key, Group: group, Label: label, Percent: p, Known: known, ResetsAt: normalizeTime(str(m["resets_at"])), Severity: sev})
	}
	return out
}

// Codex windows are described by their length: 18000s is the 5-hour session, 604800s
// the week. Only the main rate_limit is reported; per-model side limits are left out.
func codexWindows(body map[string]any, now time.Time) []Window {
	main := obj(body["rate_limit"])
	if main == nil {
		return nil
	}
	var out []Window
	for _, key := range []string{"primary_window", "secondary_window"} {
		snap := obj(main[key])
		if snap == nil {
			continue
		}
		secs, _ := num(snap["limit_window_seconds"])
		hours := int(math.Round(secs / 3600))
		session := secs > 0 && secs <= 24*3600
		span := fmt.Sprintf("%dh", hours)
		if hours >= 24 {
			span = fmt.Sprintf("%dd", int(math.Round(float64(hours)/24)))
		} else if hours == 0 {
			span = "?h"
		}
		p, known := clampPct(snap["used_percent"])
		w := Window{Key: strings.TrimSuffix(key, "_window"), Group: "weekly", Label: span + " all", Percent: p, Known: known}
		if session {
			w.Group, w.Label = "session", span+" session"
		}
		if at, ok := num(snap["reset_at"]); ok && at > 0 {
			w.ResetsAt = time.Unix(int64(at), 0).UTC().Format(time.RFC3339)
		} else if after, ok := num(snap["reset_after_seconds"]); ok {
			w.ResetsAt = now.Add(time.Duration(after) * time.Second).UTC().Format(time.RFC3339)
		}
		if allowed, ok := main["allowed"].(bool); ok && !allowed && p >= 100 {
			w.Severity = "locked"
		} else if known {
			w.Severity = severityTag(p)
		}
		out = append(out, w)
	}
	return out
}

func severityTag(p float64) string {
	if s := SeverityFor(p); s != "ok" {
		return s
	}
	return ""
}

// Headroom is the busiest session window and the busiest weekly window, in percent used.
type Headroom struct{ Session, Weekly float64 }

// Tightest is the higher of the two: what actually limits the account right now.
func (h Headroom) Tightest() float64 { return math.Max(h.Session, h.Weekly) }

// HeadroomOf summarises windows.
func HeadroomOf(ws []Window) Headroom {
	var h Headroom
	sessionSeen := false
	for _, w := range ws {
		if w.Group == "session" && !sessionSeen {
			h.Session, sessionSeen = w.Percent, true
		}
		if w.Group == "weekly" && w.Percent > h.Weekly {
			h.Weekly = w.Percent
		}
	}
	return h
}

// LoginHealth describes how long a stored login has left.
type LoginHealth struct {
	State   string `json:"state"` // ok, expiring, expired, missing, unknown
	Message string `json:"message"`
}

// HealthOf reports a result's login state.
func HealthOf(res *Result, now time.Time) LoginHealth {
	r := res.Record
	switch {
	case r.Missing:
		return LoginHealth{"missing", "no stored token — sign in again"}
	case res.NeedsLogin:
		return LoginHealth{"expired", "login expired or revoked — sign in again"}
	}
	if r.RefreshTokenExpiresAt > 0 {
		left := time.Duration(r.RefreshTokenExpiresAt-now.UnixMilli()) * time.Millisecond
		switch {
		case left <= 0:
			return LoginHealth{"expired", "login expired — sign in again"}
		case left < LoginWarn:
			return LoginHealth{"expiring", "login expires in " + FormatRelative(left)}
		}
		return LoginHealth{"ok", "login valid for " + FormatRelative(left)}
	}
	if r.Provider == Codex {
		msg := "login active"
		if r.LastRefresh > 0 {
			msg += " · token refreshed " + FormatRelative(now.Sub(msToTime(r.LastRefresh))) + " ago"
		}
		return LoginHealth{"ok", msg}
	}
	return LoginHealth{"unknown", "login lifetime unknown"}
}

var tierPattern = regexp.MustCompile(`(?i)(pro|max)_?(\d+x)?`)

// TierLabel is the plan name, e.g. "Max 20x" or "ChatGPT Pro".
func TierLabel(r *Record) string {
	if r.Provider == Codex {
		names := map[string]string{"free": "Free", "plus": "Plus", "pro": "Pro", "team": "Team", "business": "Business", "enterprise": "Enterprise", "edu": "Edu"}
		plan := strings.ToLower(r.PlanType)
		if n := names[plan]; n != "" {
			return "ChatGPT " + n
		}
		if plan != "" {
			return "ChatGPT " + strings.ToUpper(plan[:1]) + plan[1:]
		}
		return "ChatGPT"
	}
	tier := firstNonEmpty(r.RateLimitTier, str(r.Profile["organizationRateLimitTier"]))
	if m := tierPattern.FindStringSubmatch(tier); m != nil {
		label := strings.ToUpper(m[1][:1]) + strings.ToLower(m[1][1:])
		if m[2] != "" {
			label += " " + m[2]
		}
		return label
	}
	return r.SubscriptionType
}

// ExtraUsage is Claude's pay-as-you-go overflow, when enabled.
type ExtraUsage struct {
	Enabled     bool
	Used, Limit string
	Utilization float64
	HasUtil     bool
}

// ExtraUsageOf reads extra_usage (Claude) from a usage body.
func ExtraUsageOf(raw json.RawMessage) ExtraUsage {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return ExtraUsage{}
	}
	e := obj(body["extra_usage"])
	if e == nil || e["is_enabled"] != true {
		return ExtraUsage{}
	}
	out := ExtraUsage{Enabled: true, Used: "?", Limit: "∞"}
	if n, ok := num(e["used_credits"]); ok {
		out.Used = fmt.Sprintf("%.2f", n)
	}
	if n, ok := num(e["monthly_limit"]); ok {
		out.Limit = fmt.Sprintf("%.2f", n)
	}
	out.Utilization, out.HasUtil = num(e["utilization"])
	return out
}
