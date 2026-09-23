package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/getparable/aiu/internal/core"
)

func validateResetOptions(opts *options, command string) error {
	if (opts.yes || opts.resetCreditID != "" || opts.resetRequestID != "") && command != "reset" {
		return errors.New("--yes, --credit-id, and --request-id are only valid with reset")
	}
	if (opts.autoResetEnabled != "" || opts.autoResetThreshold != nil) && command != "auto-reset" {
		return errors.New("--enabled and --threshold are only valid with auto-reset")
	}
	if command == "reset" && !opts.yes {
		return errors.New("reset requires --yes: using a banked reset refreshes eligible usage windows and moves the weekly reset date; inspect `aiu resets codex:NAME` first")
	}
	if command == "auto-reset" && opts.autoResetEnabled == "" && opts.autoResetThreshold == nil {
		return errors.New("auto-reset requires --enabled true|false, --threshold N, or both")
	}
	if opts.autoResetEnabled != "" && opts.autoResetEnabled != "true" && opts.autoResetEnabled != "false" {
		return errors.New("--enabled must be true or false")
	}
	if opts.command != "frontend" && (command == "reset" || command == "resets" || command == "auto-reset") && (len(opts.args) != 1 || opts.args[0] == "") {
		return fmt.Errorf("%s needs exactly one account selector", command)
	}
	return nil
}

func (a *app) resets(ctx context.Context) error {
	view, err := a.cfg.ListBankedResets(ctx, a.opts.args[0], a.opts.provider)
	if err != nil {
		return err
	}
	if a.opts.json {
		return json.NewEncoder(a.stdout).Encode(view)
	}
	if _, err := fmt.Fprintln(a.stdout, a.p.bold("Banked resets · "+a.opts.args[0])); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, strings.Join(renderBankedResets(a.p, view, true), "\n"))
	return err
}

func (a *app) reset(ctx context.Context) error {
	result, err := a.cfg.ConsumeBankedReset(ctx, a.opts.args[0], a.opts.provider, a.opts.resetCreditID, a.opts.resetRequestID)
	if err != nil {
		return err
	}
	if a.opts.json {
		return json.NewEncoder(a.stdout).Encode(result)
	}
	if _, err := fmt.Fprintln(a.stdout, result.Message); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, a.p.dim("Request: "+result.RequestID+". Reuse --request-id to query this same attempt."))
	return err
}

func (a *app) autoReset(ctx context.Context) error {
	if err := a.cfg.ConfigureAutoReset(ctx, a.opts.args[0], a.opts.provider, autoResetEnabled(a.opts), a.opts.autoResetThreshold); err != nil {
		return err
	}
	if a.opts.json {
		return json.NewEncoder(a.stdout).Encode(struct {
			Account   string `json:"account"`
			Enabled   *bool  `json:"autoReset,omitempty"`
			Threshold *int   `json:"autoResetThresholdPercent,omitempty"`
		}{a.opts.args[0], autoResetEnabled(a.opts), a.opts.autoResetThreshold})
	}
	_, err := fmt.Fprintln(a.stdout, autoResetMessage(a.opts)+" for "+a.opts.args[0]+". Checks run while AIU collects usage.")
	return err
}

func autoResetEnabled(opts *options) *bool {
	if opts.autoResetEnabled == "" {
		return nil
	}
	enabled := opts.autoResetEnabled == "true"
	return &enabled
}

func autoResetMessage(opts *options) string {
	message := "Auto reset settings saved"
	if opts.autoResetEnabled == "true" {
		message = "Auto reset enabled"
	}
	if opts.autoResetEnabled == "false" {
		message = "Auto reset disabled"
	}
	if opts.autoResetThreshold != nil {
		message += fmt.Sprintf("; threshold set to %d%% remaining", *opts.autoResetThreshold)
	}
	return message
}

func renderBankedResets(p painter, view *core.BankedResets, details bool) []string {
	count := "unknown"
	if view.AvailableCount != nil {
		count = fmt.Sprintf("%d available", *view.AvailableCount)
	}
	line := "  banked resets  " + count
	if view.AutoReset {
		line += fmt.Sprintf(" · auto at %d%% remaining", view.AutoResetThresholdPercent)
	} else {
		line += fmt.Sprintf(" · auto off (threshold %d%% remaining)", view.AutoResetThresholdPercent)
	}
	lines := []string{line}
	if view.AutoResetStatus != "" {
		lines = append(lines, "  "+p.dim(view.AutoResetStatus))
	}
	if view.PendingRequest != nil {
		lines = append(lines, "  "+p.yellow("Reset outcome pending; retry reconciles the same request "+view.PendingRequest.RequestID))
	}
	for _, note := range []string{view.Stale, view.Error} {
		if note != "" {
			lines = append(lines, "  "+p.yellow(note))
		}
	}
	if details {
		if view.Credits == nil {
			lines = append(lines, "  Reset details are unavailable.")
		}
		for _, credit := range view.Credits {
			expiry := credit.ExpiresAt
			if expiry == "" {
				expiry = "not reported"
			}
			lines = append(lines, fmt.Sprintf("  %s · %s · expires %s", credit.ID, credit.Status, expiry))
		}
	}
	return lines
}
