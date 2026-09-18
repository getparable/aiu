package core

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Which scoped weekly window stands for "the model I actually came here to use". The
// flagship is the first of these an account reports; an account that reports none while
// its siblings do has no flagship access at all, which is what being out of it feels like.
var flagshipModels = []string{"fable", "opus"}

// planMultiplier reads Claude's own plan arithmetic out of the tier name: "Max 20x" is
// twenty Pro plans' worth of capacity, "Max 5x" five.
var planMultiplier = regexp.MustCompile(`(?i)(\d+)\s*x\b`)

// PlanWeight turns a tier label into what one percent of its weekly window is worth. Only
// a multiplier the plan states itself counts; every other plan — Pro, a ChatGPT tier, an
// account whose tier never loaded — weighs one, so those rank against each other on
// percentages alone rather than on a number we invented for them.
func PlanWeight(tier string) float64 {
	if m := planMultiplier.FindStringSubmatch(tier); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// isScopedWeekly reports whether a weekly window covers one model rather than everything.
func isScopedWeekly(w Window) bool {
	if w.Group != "weekly" {
		return false
	}
	return strings.Contains(w.Key, ":") || w.Key == "seven_day_opus" || w.Key == "seven_day_sonnet" || w.Key == "seven_day_oauth_apps"
}

// scopeName is the model or surface a scoped window covers, e.g. "Fable".
func scopeName(w Window) string {
	if _, after, ok := strings.Cut(w.Key, ":"); ok {
		return after
	}
	return strings.TrimPrefix(w.Label, "7d ")
}

// Fitness is what the ranking measured on one account. Percentages stay percentages here;
// Score is the one derived number, and it is capacity rather than percentage — a nearly
// full Max 20x carries more work than an untouched Pro plan.
type Fitness struct {
	Spent        bool    // the general weekly window is gone: nothing to spend here at all
	Score        float64 // weekly percent left × plan weight
	Session      float64 // percent of the 5h window used
	Weekly       float64 // percent of the general weekly window used
	Flagship     float64 // percent of the flagship weekly window used; -1 when there is none
	FlagshipName string  // what that window covers, e.g. "Fable"
	Weight       float64 // the plan weight that scaled Score
	Tier         string
}

// SessionFree is true while the 5h window still has room. No session window at all — as
// Codex reports — counts as free, since nothing is gating the next request.
func (f Fitness) SessionFree() bool { return f.Session < 100 }

// FitnessOf measures one account's windows under its plan.
func FitnessOf(tier string, ws []Window) Fitness {
	f := Fitness{Flagship: -1, Weight: PlanWeight(tier), Tier: tier}
	for _, w := range ws {
		switch {
		case w.Group == "session":
			f.Session = math.Max(f.Session, w.Percent)
		case isScopedWeekly(w):
			name := scopeName(w)
			if contains(flagshipModels, strings.ToLower(name)) && w.Percent > f.Flagship {
				f.Flagship, f.FlagshipName = w.Percent, name
			}
		case w.Group == "weekly":
			f.Weekly = math.Max(f.Weekly, w.Percent)
		}
	}
	f.Spent = f.Weekly >= 100
	f.Score = (100 - f.Weekly) * f.Weight
	return f
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// band places an account in the ranking's coarse tiers, best first. The two gates come
// before capacity on purpose, so no plan size can buy its way past a limit that is shut.
// flagshipMatters is false when no account in the comparison reports a flagship window —
// Codex reports none — and the gate then holds for nobody rather than against everybody.
func band(f Fitness, flagshipMatters bool) int {
	flagshipFree := !flagshipMatters || (f.Flagship >= 0 && f.Flagship < 100)
	switch {
	case f.SessionFree() && flagshipFree:
		return 0
	case f.SessionFree():
		return 1
	case flagshipFree:
		return 2
	}
	return 3
}

// reason says, in one line, what the ranking saw: how much of the week is left, on what
// plan, and whichever of the two gates is currently shut.
func reason(f Fitness, flagshipMatters bool) string {
	parts := []string{fmt.Sprintf("%.0f%% of the week left", 100-f.Weekly)}
	if f.Tier != "" {
		parts = append(parts, f.Tier)
	}
	if flagshipMatters {
		switch name := firstNonEmpty(f.FlagshipName, "Fable"); {
		case f.Flagship < 0:
			parts = append(parts, "no "+name)
		case f.Flagship >= 100:
			parts = append(parts, name+" spent")
		}
	}
	if !f.SessionFree() {
		parts = append(parts, "5h limit hit")
	}
	return strings.Join(parts, " · ")
}

// Pick is the account to work in next, and why.
type Pick struct {
	Result   *Result
	Fit      Fitness
	Band     int
	Reason   string
	AllSpent bool // every account's weekly window is gone; this one simply comes back first
}

// Recommend chooses the account to work in next out of one provider's accounts, across
// every address: the one whose 5h window is free, whose flagship model is still there,
// and which has the most weekly capacity left.
//
// When every account is out of weekly room it names the one that frees up first instead,
// with AllSpent set, so callers can say so rather than recommend a dead account.
func Recommend(results []*Result, now time.Time) *Pick {
	type scored struct {
		res *Result
		fit Fitness
	}
	var in []scored
	flagshipMatters := false
	for _, r := range results {
		if !switchable(r) {
			continue
		}
		fit := FitnessOf(TierLabel(r.Record), NormalizeWindows(r.Usage, now))
		if fit.Flagship >= 0 {
			flagshipMatters = true
		}
		in = append(in, scored{r, fit})
	}
	if len(in) == 0 {
		return nil
	}
	best := in[0]
	bestBand := band(best.fit, flagshipMatters)
	for _, c := range in[1:] {
		b := band(c.fit, flagshipMatters)
		if betterThan(c.fit, b, best.fit, bestBand) {
			best, bestBand = c, b
		}
	}
	pick := &Pick{Result: best.res, Fit: best.fit, Band: bestBand}
	if best.fit.Spent {
		pick.AllSpent = true
		if soonest := firstToReset(results, now); soonest != nil {
			pick.Result = soonest
			pick.Fit = FitnessOf(TierLabel(soonest.Record), NormalizeWindows(soonest.Usage, now))
			pick.Band = band(pick.Fit, flagshipMatters)
		}
	}
	pick.Reason = reason(pick.Fit, flagshipMatters)
	return pick
}

// switchable is an account the recommendation may name: one that can actually be switched
// to and whose numbers are trustworthy.
func switchable(r *Result) bool {
	return r.Err == "" && !r.NeedsLogin && !r.Record.Missing && !r.Record.IsReadOnly()
}

// betterThan orders two ranked accounts: anything with room beats anything spent, then
// the gates, then capacity, then the emptier 5h window.
func betterThan(a Fitness, aBand int, b Fitness, bBand int) bool {
	if a.Spent != b.Spent {
		return !a.Spent
	}
	if aBand != bBand {
		return aBand < bBand
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.Session < b.Session
}

// firstToReset names the account whose general weekly window comes back soonest.
func firstToReset(results []*Result, now time.Time) *Result {
	type candidate struct {
		res *Result
		at  time.Time
	}
	var cands []candidate
	for _, r := range results {
		if !switchable(r) {
			continue
		}
		for _, w := range NormalizeWindows(r.Usage, now) {
			if w.Group != "weekly" || isScopedWeekly(w) {
				continue
			}
			if at, ok := w.ResetTime(); ok && at.After(now) {
				cands = append(cands, candidate{r, at})
			}
			break
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].at.Before(cands[j].at) })
	return cands[0].res
}
