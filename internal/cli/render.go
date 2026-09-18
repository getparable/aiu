package cli

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/getparable/aiu/internal/core"
)

const barWidth = 20

type painter struct{ on bool }

func (p painter) paint(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p painter) bold(s string) string   { return p.paint("1", s) }
func (p painter) dim(s string) string    { return p.paint("2", s) }
func (p painter) green(s string) string  { return p.paint("32", s) }
func (p painter) yellow(s string) string { return p.paint("33", s) }
func (p painter) red(s string) string    { return p.paint("31", s) }
func (p painter) cyan(s string) string   { return p.paint("36", s) }

func (p painter) forPercent(pct float64, s string) string {
	switch core.SeverityFor(pct) {
	case "critical":
		return p.red(s)
	case "warning":
		return p.yellow(s)
	}
	return p.green(s)
}

func (p painter) tag(pr core.Provider) string {
	text := pr.Glyph() + " " + pr.Name()
	if pr == core.Codex {
		return p.cyan(text)
	}
	return p.yellow(text)
}

func (p painter) bar(w core.Window) string {
	if !w.Known {
		return p.dim(strings.Repeat("─", barWidth))
	}
	filled := int(math.Round(w.Percent / 100 * barWidth))
	return p.forPercent(w.Percent, strings.Repeat("█", filled)) + p.dim(strings.Repeat("░", barWidth-filled))
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// padVisible pads by displayed width: colour codes count for nothing, wide characters two.
func padVisible(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-core.DisplayWidth(ansi.ReplaceAllString(s, ""))))
}

func renderAccount(p painter, res *core.Result, labelWidth int, tagged bool, now time.Time) []string {
	r := res.Record
	marker := p.dim("○")
	if res.Active {
		marker = p.green("●")
	}
	tag := ""
	if tagged {
		tag = p.tag(r.Provider) + " "
	}
	title := fmt.Sprintf("%s %s%s %s", marker, tag, p.bold(padVisible(r.Label, labelWidth)), p.dim(r.Email))

	health := core.HealthOf(res, now)
	var note string
	switch health.State {
	case "expired", "missing":
		flag := ""
		if r.Provider == core.Codex {
			flag = " --codex"
		}
		note = p.red(fmt.Sprintf("%s → aiu login%s --label %s", health.Message, flag, r.Label))
	case "expiring":
		note = p.yellow(health.Message)
	default:
		note = p.dim(health.Message)
	}
	var meta []string
	if name := core.DistinctOrgName(r); name != "" {
		meta = append(meta, name)
	}
	if t := core.TierLabel(r); t != "" {
		meta = append(meta, t)
	}
	if res.Active {
		meta = append(meta, p.green("active in "+r.Provider.Client()))
	}
	meta = append(meta, note)
	lines := []string{title + "  " + strings.Join(meta, p.dim(" · "))}

	if res.Err != "" {
		return append(lines, "  "+p.red("✖")+" "+res.Err)
	}
	if res.Stale != "" {
		lines = append(lines, "  "+p.yellow("!")+" "+p.dim(res.Stale))
	}
	windows := core.NormalizeWindows(res.Usage, now)
	lw := 10
	for _, w := range windows {
		lw = max(lw, len(w.Label))
	}
	for _, w := range windows {
		pct := p.dim("  n/a")
		if w.Known {
			pct = p.forPercent(w.Percent, fmt.Sprintf("%3.0f%%", w.Percent))
		}
		var parts []string
		if at, ok := w.ResetTime(); ok {
			parts = append(parts, p.dim(fmt.Sprintf("resets in %s  (%s)", core.FormatRelative(at.Sub(now)), core.FormatLocal(at))))
		}
		if w.Severity == "locked" {
			parts = append(parts, p.red("[locked]"))
		}
		lines = append(lines, fmt.Sprintf("  %-*s %s  %s   %s", lw, w.Label, p.bar(w), pct, strings.Join(parts, "  ")))
	}
	if extra := core.ExtraUsageOf(res.Usage); extra.Enabled {
		util := ""
		if extra.HasUtil {
			util = fmt.Sprintf("  (%.0f%%)", extra.Utilization)
		}
		lines = append(lines, fmt.Sprintf("  %-*s %s", lw, "extra usage", p.dim("$"+extra.Used+" / $"+extra.Limit+util)))
	}
	return lines
}

// summarize names the account to work in next and the next 5h reset.
func summarize(p painter, group []*core.Result, named bool, now time.Time) []string {
	var ok []*core.Result
	for _, r := range group {
		if r.Err == "" {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	who := ""
	if named {
		who = ok[0].Record.Provider.Name() + " "
	}
	var lines []string
	if pick := core.Recommend(ok, now); pick != nil {
		lead := "use next"
		if pick.AllSpent {
			lead = "all spent — back first"
		}
		lines = append(lines, fmt.Sprintf("%s %s%s: %s %s", p.cyan("\u2192"), who, lead,
			p.bold(pick.Result.Record.Label), p.dim("("+pick.Reason+")")))
	}

	var next *core.Result
	var nextAt time.Time
	for _, r := range ok {
		for _, w := range core.NormalizeWindows(r.Usage, now) {
			if w.Group != "session" {
				continue
			}
			if at, has := w.ResetTime(); has && w.Percent > 0 && (next == nil || at.Before(nextAt)) {
				next, nextAt = r, at
			}
			break
		}
	}
	if next != nil {
		lines = append(lines, fmt.Sprintf("%s %snext 5h reset: %s %s", p.cyan("\u2192"), who, p.bold(next.Record.Label), p.dim("in "+core.FormatRelative(nextAt.Sub(now)))))
	}
	return lines
}

func render(p painter, snap *core.Snapshot, now time.Time) string {
	labelWidth := 4
	for _, r := range snap.Results {
		labelWidth = max(labelWidth, core.DisplayWidth(r.Record.Label))
	}
	type group struct {
		provider core.Provider
		results  []*core.Result
	}
	var groups []group
	for _, pr := range core.Providers {
		var g []*core.Result
		for _, r := range snap.Results {
			if r.Record.Provider == pr {
				g = append(g, r)
			}
		}
		if len(g) > 0 {
			groups = append(groups, group{pr, g})
		}
	}
	several := len(groups) > 1
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.provider.Name()
	}
	zone, _ := now.Zone()
	out := []string{p.bold(strings.Join(names, " · ")+" usage") + p.dim(fmt.Sprintf("  %s  (%s)", core.FormatLocal(now), zone)), ""}
	for _, g := range groups {
		if several {
			out = append(out, p.bold(p.tag(g.provider)), "")
		}
		for _, r := range g.results {
			out = append(out, renderAccount(p, r, labelWidth, !several, now)...)
			out = append(out, "")
		}
		out = append(out, summarize(p, g.results, several, now)...)
		if several {
			out = append(out, "")
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// jsonAccount is the stable --json shape, for scripts and status bars.
type jsonAccount struct {
	Provider         core.Provider    `json:"provider"`
	Email            string           `json:"email"`
	Label            string           `json:"label"`
	Org              string           `json:"org,omitempty"`
	OrgName          string           `json:"orgName,omitempty"`
	Active           bool             `json:"active"`
	Tier             string           `json:"tier,omitempty"`
	SubscriptionType string           `json:"subscriptionType,omitempty"`
	RateLimitTier    string           `json:"rateLimitTier,omitempty"`
	PlanType         string           `json:"planType,omitempty"`
	TokenExpiresAt   string           `json:"tokenExpiresAt,omitempty"`
	LoginExpiresAt   string           `json:"loginExpiresAt,omitempty"`
	Login            core.LoginHealth `json:"login"`
	ReadOnly         bool             `json:"readOnly"`
	CanSwitch        bool             `json:"canSwitch"`
	Windows          []core.Window    `json:"windows"`
	Usage            any              `json:"usage"`
	Stale            string           `json:"stale,omitempty"`
	FetchedAt        string           `json:"fetchedAt,omitempty"`
	Error            string           `json:"error,omitempty"`
	// Recommended marks the one account per provider worth switching to next, and Why
	// says what earned it. AllSpent turns the recommendation into a countdown: nothing
	// has weekly room left, and this is merely the account that comes back first.
	Recommended bool   `json:"recommended,omitempty"`
	Why         string `json:"why,omitempty"`
	AllSpent    bool   `json:"allSpent,omitempty"`
}

func toJSON(snap *core.Snapshot, now time.Time) []jsonAccount {
	iso := func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	picks := map[core.Provider]*core.Pick{}
	for _, p := range core.Providers {
		var group []*core.Result
		for _, res := range snap.Results {
			if res.Record.Provider == p {
				group = append(group, res)
			}
		}
		if pick := core.Recommend(group, now); pick != nil {
			picks[p] = pick
		}
	}
	out := make([]jsonAccount, 0, len(snap.Results))
	for _, res := range snap.Results {
		r := res.Record
		windows := core.NormalizeWindows(res.Usage, now)
		if windows == nil {
			windows = []core.Window{}
		}
		var usage any
		if len(res.Usage) > 0 {
			usage = res.Usage
		}
		out = append(out, jsonAccount{
			Provider: r.Provider, Email: r.Email, Label: r.Label, Org: r.OrgUUID, OrgName: r.OrgName, Active: res.Active,
			Tier: core.TierLabel(r), SubscriptionType: r.SubscriptionType, RateLimitTier: r.RateLimitTier, PlanType: r.PlanType,
			TokenExpiresAt: iso(r.ExpiresAt), LoginExpiresAt: iso(r.RefreshTokenExpiresAt),
			Login: core.HealthOf(res, now), ReadOnly: r.IsReadOnly(), CanSwitch: !r.IsReadOnly() && !r.Missing,
			Windows: windows, Usage: usage, Stale: res.Stale, FetchedAt: iso(res.FetchedAt), Error: res.Err,
		})
		if pick := picks[r.Provider]; pick != nil && pick.Result == res {
			last := &out[len(out)-1]
			last.Recommended, last.Why, last.AllSpent = true, pick.Reason, pick.AllSpent
		}
	}
	return out
}

// sortResults orders by the busiest 5h or 7d window, errors last.
func sortResults(results []*core.Result, mode string, now time.Time) {
	if mode != "5h" && mode != "7d" {
		return
	}
	key := func(r *core.Result) float64 {
		if r.Err != "" {
			return 999
		}
		h := core.HeadroomOf(core.NormalizeWindows(r.Usage, now))
		if mode == "7d" {
			return h.Weekly
		}
		return h.Session
	}
	sort.SliceStable(results, func(i, j int) bool { return key(results[i]) < key(results[j]) })
}
