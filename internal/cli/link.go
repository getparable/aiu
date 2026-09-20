package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

type LinkState struct {
	State  string `json:"state"`
	Path   string `json:"path"`
	Target string `json:"target"`
	Binary string `json:"binary"`
	OnPath bool   `json:"onPath"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func currentBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolveBinary(exe)
}

func readLinkState() LinkState {
	s := LinkState{Path: linkPath(), State: "missing"}
	s.Binary, _ = currentBinary()
	info, err := os.Lstat(s.Path)
	switch {
	case os.IsNotExist(err):
		s.Detail = "not installed"
	case err != nil:
		s.State, s.Detail = "occupied", err.Error()
	case !isOwnedLink(info):
		s.State, s.Detail = "occupied", s.Path+" exists and is not an aiu installation — remove it yourself first"
	default:
		s.Target = linkTarget()
		if sameInstallation(s.Target, s.Binary) {
			s.State, s.Detail = "installed", "`aiu` runs this build"
		} else {
			s.State, s.Detail = "elsewhere", s.Path+" points at "+s.Target
		}
	}
	s.OnPath = dirOnLoginPath(linkDir())
	return s
}

func (a *app) link(context.Context) error {
	var err error
	switch {
	case len(a.opts.args) == 0 || a.opts.args[0] == "install":
		err = installLink()
	case a.opts.args[0] == "remove":
		err = removeLink()
	case a.opts.args[0] == "status":
	default:
		return fmt.Errorf("usage: aiu link [install|remove|status] [--json]")
	}
	s := readLinkState()
	if err != nil {
		s.Error = err.Error()
	}
	if a.opts.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if e := enc.Encode(s); e != nil {
			return e
		}
		return err
	}
	if err != nil {
		return err
	}
	if s.State == "installed" {
		fmt.Fprintf(a.stdout, "%s %s → %s\n", a.p.green("✔"), s.Path, s.Binary)
		if !s.OnPath {
			fmt.Fprintln(a.stdout, a.p.yellow("!")+" "+linkDir()+" is not on your PATH — add it in your shell profile")
		}
	} else {
		fmt.Fprintln(a.stdout, a.p.dim(s.Detail))
	}
	return nil
}
