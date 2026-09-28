package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/getparable/aiu/internal/core"
)

type keychainReport struct {
	Items []core.KeychainAccess `json:"items"`
	Error string                `json:"error,omitempty"`
}

var keychainTitles = map[string]string{"claude": "Claude Code login", "aiu": "AIU accounts"}

// keychain reports, without prompting, which Keychain items macOS would ask
// about; `allow` reads one so the prompt appears when the user expects it.
func (a *app) keychain(context.Context) error {
	var err error
	switch {
	case len(a.opts.args) == 0 || (len(a.opts.args) == 1 && a.opts.args[0] == "status"):
	case len(a.opts.args) == 2 && a.opts.args[0] == "allow":
		err = a.cfg.AllowKeychain(a.opts.args[1])
	default:
		return fmt.Errorf("usage: aiu keychain [status | allow claude|aiu] [--json]")
	}
	report := keychainReport{Items: a.cfg.KeychainAccess()}
	if err != nil {
		report.Error = core.Redact(err.Error())
	}
	if a.opts.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if e := enc.Encode(report); e != nil {
			return e
		}
		return err
	}
	for _, item := range report.Items {
		mark := a.p.dim("·")
		switch item.State {
		case core.AccessGranted:
			mark = a.p.green("✔")
		case core.AccessNeedsApproval, core.AccessUnknown:
			mark = a.p.yellow("!")
		}
		line := fmt.Sprintf("%s %-18s %s", mark, keychainTitles[item.ID], item.State)
		if item.Detail != "" {
			line += a.p.dim(" — " + item.Detail)
		}
		fmt.Fprintln(a.stdout, line)
		if item.State == core.AccessNeedsApproval {
			fmt.Fprintln(a.stdout, a.p.dim("  run `aiu keychain allow "+item.ID+"`, enter your Mac password, and choose Always Allow"))
		}
	}
	return err
}
