package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LinkDir is where the `aiu` command is linked from: on PATH for most shells, and
// writable without administrator rights.
func linkDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

func linkPath() string { return filepath.Join(linkDir(), "aiu") }

// LinkState describes the `aiu` shortcut, for the CLI and the menu bar's Settings pane.
type LinkState struct {
	State  string `json:"state"` // installed, elsewhere, occupied, missing
	Path   string `json:"path"`
	Target string `json:"target"`           // where the link points, when it is one
	Binary string `json:"binary"`           // the binary running now
	OnPath bool   `json:"onPath"`           // is the directory on the login shell's PATH
	Error  string `json:"error,omitempty"`  // why an install or removal failed
	Detail string `json:"detail,omitempty"` // one line for a person
}

// currentBinary is this executable, following any symlink, so a link never points at
// another link.
func currentBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func readLinkState() LinkState {
	state := LinkState{Path: linkPath(), State: "missing"}
	state.Binary, _ = currentBinary()
	info, err := os.Lstat(state.Path)
	switch {
	case err != nil:
		state.Detail = "not installed"
	case info.Mode()&os.ModeSymlink == 0:
		state.State, state.Detail = "occupied", state.Path+" exists and is not a link — remove it yourself first"
	default:
		state.Target, _ = os.Readlink(state.Path)
		if resolved, err := filepath.EvalSymlinks(state.Path); err == nil {
			state.Target = resolved
		}
		if state.Target == state.Binary {
			state.State, state.Detail = "installed", "`aiu` runs this build"
		} else {
			state.State, state.Detail = "elsewhere", state.Path+" points at "+state.Target
		}
	}
	state.OnPath = dirOnLoginPath(linkDir())
	return state
}

// dirOnLoginPath asks the login shell, because a GUI app inherits a minimal PATH that
// says nothing about what the user's terminal sees.
func dirOnLoginPath(dir string) bool {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	out, err := exec.Command(shell, "-lic", `printf %s "$PATH"`).Output()
	if err != nil {
		out = []byte(os.Getenv("PATH"))
	}
	for _, p := range strings.Split(string(out), ":") {
		if strings.TrimSpace(p) == dir {
			return true
		}
	}
	return false
}

func installLink() error {
	binary, err := currentBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(linkDir(), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(linkPath()); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a link — remove it yourself first", linkPath())
		}
		if err := os.Remove(linkPath()); err != nil {
			return err
		}
	}
	return os.Symlink(binary, linkPath())
}

func removeLink() error {
	info, err := os.Lstat(linkPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s is not a link — leaving it alone", linkPath())
	}
	return os.Remove(linkPath())
}

// link installs, removes or reports the `aiu` shortcut. The menu bar app calls it with
// --json so one download installs both the panel and the command.
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
	state := readLinkState()
	if err != nil {
		state.Error = err.Error()
	}
	if a.opts.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if encodeErr := enc.Encode(state); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if err != nil {
		return err
	}
	switch state.State {
	case "installed":
		fmt.Fprintf(a.stdout, "%s %s → %s\n", a.p.green("✔"), state.Path, state.Binary)
		if !state.OnPath {
			fmt.Fprintln(a.stdout, a.p.yellow("!")+" "+linkDir()+" is not on your PATH — add it in your shell profile")
		}
	default:
		fmt.Fprintln(a.stdout, a.p.dim(state.Detail))
	}
	return nil
}
