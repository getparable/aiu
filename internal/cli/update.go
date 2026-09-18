package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getparable/aiu/internal/core"
)

// update reports whether a newer release exists, and prints the command that would
// install it. It never installs anything itself: whoever owns this binary — Homebrew,
// or the clone it was built from — owns replacing it too.
//
// The menu bar app calls this with --json for its Settings pane, so the check, its
// cache and its rate limiting live here rather than in two front ends.
func (a *app) update(ctx context.Context) error {
	if len(a.opts.args) > 0 && a.opts.args[0] != "check" {
		return fmt.Errorf("usage: aiu update [check] [--force] [--json]")
	}

	state := a.cfg.CheckUpdate(ctx, a.opts.force)

	if a.opts.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}

	switch state.State {
	case "available":
		fmt.Fprintf(a.stdout, "%s %s\n", a.p.yellow("↑"), state.Detail)
		if state.Command != "" {
			fmt.Fprintf(a.stdout, "\n  %s\n\n", a.p.cyan(state.Command))
		} else {
			fmt.Fprintf(a.stdout, "\n  %s\n\n", a.p.dim(state.URL))
		}
	case "current":
		fmt.Fprintf(a.stdout, "%s %s\n", a.p.green("✔"), state.Detail)
	case "development":
		fmt.Fprintln(a.stdout, a.p.dim(state.Detail))
	default:
		fmt.Fprintf(a.stdout, "%s %s\n", a.p.yellow("!"), state.Detail)
	}

	// A stale answer still printed above, so this is a footnote rather than the news.
	if state.Error != "" && state.State != "unknown" {
		fmt.Fprintln(a.stdout, a.p.dim("last check failed: "+state.Error))
	}
	if state.Error != "" && state.State == "unknown" {
		fmt.Fprintln(a.stderr, a.p.dim(state.Error))
	}
	if state.Cached && state.State != "development" {
		age := core.FormatRelative(a.cfg.Now().Sub(state.CheckedAt))
		fmt.Fprintln(a.stdout, a.p.dim("checked "+age+" ago; --force asks GitHub again"))
	}
	return nil
}
