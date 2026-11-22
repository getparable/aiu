// Package cli is aiu's terminal front end.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getparable/aiu/internal/core"
)

type options struct {
	command            string
	args               []string
	json               bool
	provider           core.Provider
	label              string
	interval           int
	sortMode           string
	noSync             bool
	readOnly           bool
	manual             bool
	console            bool
	noOpen             bool
	noColor            bool
	force              bool
	contractVersion    string
	resetCreditID      string
	resetRequestID     string
	autoResetEnabled   string
	autoResetThreshold *int
	yes                bool
	help               bool
	version            bool
}

var valueFlags = map[string]bool{"label": true, "interval": true, "sort": true, "provider": true, "contract-version": true, "credit-id": true, "request-id": true, "enabled": true, "threshold": true}

func parse(argv []string) (*options, error) {
	o := &options{}
	var positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-h":
			o.help = true
			continue
		case a == "-v" || a == "-V":
			o.version = true
			continue
		case !strings.HasPrefix(a, "--"):
			positional = append(positional, a)
			continue
		}
		key, value, hasValue := strings.Cut(a[2:], "=")
		if valueFlags[key] && !hasValue {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("--%s needs a value", key)
			}
			i++
			value = argv[i]
		}
		switch key {
		case "contract-version":
			o.contractVersion = value
		case "credit-id":
			o.resetCreditID = value
		case "request-id":
			o.resetRequestID = value
		case "enabled":
			if value != "true" && value != "false" {
				return nil, errors.New("--enabled must be true or false")
			}
			o.autoResetEnabled = value
		case "threshold":
			threshold, err := strconv.Atoi(value)
			if err != nil || threshold < 0 || threshold > 99 {
				return nil, errors.New("--threshold must be a whole remaining percentage from 0 to 99")
			}
			o.autoResetThreshold = &threshold
		case "yes":
			if hasValue {
				return nil, errors.New("--yes does not take a value")
			}
			o.yes = true
		case "json":
			o.json = true
		case "codex", "claude":
			if o.provider == "" {
				o.provider = core.Provider(key)
			}
		case "agy", "antigravity":
			if o.provider == "" {
				o.provider = core.Antigravity
			}
		case "provider":
			p, ok := core.ParseProvider(strings.ToLower(value))
			if !ok {
				return nil, fmt.Errorf("unknown provider %q — use claude, codex, or agy", value)
			}
			o.provider = p
		case "label":
			o.label = value
		case "interval":
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("--interval takes seconds, got %q", value)
			}
			o.interval = n
		case "sort":
			o.sortMode = value
		case "no-sync":
			o.noSync = true
		case "readonly":
			o.readOnly = true
		case "manual":
			o.manual = true
		case "console":
			o.console = true
		case "no-open":
			o.noOpen = true
		case "no-color":
			o.noColor = true
		case "force":
			o.force = true
		case "help":
			o.help = true
		case "version":
			o.version = true
		default:
			return nil, fmt.Errorf("unknown flag --%s", key)
		}
	}
	o.command = "status"
	if len(positional) > 0 {
		o.command, o.args = positional[0], positional[1:]
	}
	return o, nil
}

type app struct {
	cfg    *core.Config
	opts   *options
	p      painter
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// Run executes argv and returns the process exit code.
func Run(argv []string) int {
	return run(argv, core.DefaultConfig())
}

func run(argv []string, cfg *core.Config) int {
	opts, err := parse(argv)
	if err != nil {
		if len(argv) > 0 && argv[0] == "frontend" {
			enc := json.NewEncoder(os.Stdout)
			_ = enc.Encode(frontendEvent{Version: 1, Event: "hello", Capabilities: frontendCapabilities})
			_ = enc.Encode(frontendEvent{Version: 1, Event: "result", Error: &frontendError{Code: "invalid_input", Message: "invalid frontend arguments"}})
			return 2
		}
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return 2
	}
	if opts.command == "frontend" {
		return runFrontend(opts, cfg, os.Stdin, os.Stdout)
	}
	colour := !opts.noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	a := &app{cfg: cfg, opts: opts, p: painter{on: colour}, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	a.cfg.Warn = func(m string) { fmt.Fprintln(a.stderr, a.p.yellow("warn: "+core.Redact(m))) }
	a.cfg.Info = func(m string) {
		if !opts.json {
			fmt.Fprintln(a.stderr, a.p.dim(m))
		}
	}
	if opts.version {
		fmt.Fprintln(a.stdout, "aiu "+core.Version)
		return 0
	}
	commands := map[string]func(context.Context) error{
		"status": a.status, "watch": a.watch, "login": a.login, "add": a.add,
		"hook": a.hook,
		"list": a.list, "ls": a.list, "remove": a.remove, "rm": a.remove,
		"sync": a.sync, "switch": a.switchTo, "use": a.switchTo, "whoami": a.whoami,
		"link":             a.link,
		"update":           a.update,
		"migrate-keychain": a.migrateKeychain,
		"keychain":         a.keychain,
		"menubar":          a.menuBar, "gui": a.menuBar,
		"resets": a.resets, "reset": a.reset, "auto-reset": a.autoReset,
		"help": func(context.Context) error { a.help(); return nil },
	}
	handler, ok := commands[opts.command]
	if opts.help || !ok {
		if !ok {
			fmt.Fprintln(a.stderr, a.p.red("unknown command: "+opts.command)+"\n")
		}
		a.help()
		if !ok {
			return 2
		}
		return 0
	}
	if err := validateResetOptions(opts, opts.command); err != nil {
		if _, writeErr := fmt.Fprintln(a.stderr, "error: "+err.Error()); writeErr != nil {
			return 2
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// The first Ctrl-C cancels the command; restoring default handling lets a
	// second one exit at once if the command must finish critical work first.
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := handler(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		fmt.Fprintln(a.stderr, a.p.red("error: "+core.Redact(err.Error())))
		return 1
	}
	return 0
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (a *app) provider() core.Provider {
	if a.opts.provider == "" {
		return core.Claude
	}
	return a.opts.provider
}

func (a *app) gather(ctx context.Context) (*core.Snapshot, error) {
	opts := core.CollectOptions{NoSync: a.opts.noSync}
	if a.opts.provider != "" {
		opts.Providers = []core.Provider{a.opts.provider}
	}
	snap, err := a.cfg.Collect(ctx, opts)
	if err != nil {
		return nil, err
	}
	if a.opts.json {
		return snap, nil
	}
	if snap.Empty {
		return nil, errors.New("no accounts tracked yet — run `aiu add` (the account Claude Code uses now) or `aiu login`")
	}
	if len(snap.Results) == 0 {
		return nil, fmt.Errorf("no %s accounts tracked yet", a.opts.provider.Name())
	}
	sortResults(snap.Results, a.opts.sortMode, time.Now())
	return snap, nil
}

func (a *app) status(ctx context.Context) error {
	snap, err := a.gather(ctx)
	if err != nil {
		return err
	}
	if a.opts.json {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(toJSON(snap, time.Now()))
	}
	fmt.Fprintln(a.stdout, render(a.p, snap, time.Now()))
	return nil
}

func (a *app) watch(ctx context.Context) error {
	interval := time.Duration(max(15, a.opts.interval)) * time.Second
	if a.opts.interval == 0 {
		interval = time.Minute
	}
	footer := a.p.dim(fmt.Sprintf("redrawing every %s · requests go out at most every %.0f min per account · ctrl+c to quit",
		interval, core.MinFetchSpacing.Minutes()))
	fmt.Fprint(a.stdout, "\x1b[2J\x1b[H")
	for {
		var body string
		if snap, err := a.gather(ctx); err != nil {
			body = a.p.red("error: " + core.Redact(err.Error()))
		} else {
			body = render(a.p, snap, time.Now())
		}
		// Home the cursor and clear each line's tail instead of wiping the screen.
		lines := strings.Split(body+"\n\n"+footer, "\n")
		fmt.Fprint(a.stdout, "\x1b[H"+strings.Join(lines, "\x1b[K\n")+"\x1b[K\n\x1b[J")
		select {
		case <-ctx.Done():
			fmt.Fprintln(a.stdout)
			return nil
		case <-time.After(interval):
		}
	}
}

func (a *app) reportSaved(s *core.SavedAccount) {
	r := s.Record
	verb := "updated"
	if s.IsNew {
		verb = "added"
	}
	tier := core.TierLabel(r)
	if tier == "" {
		tier = "unknown tier"
	}
	fmt.Fprintf(a.stdout, "%s %s %s %s %s %s\n", a.p.green("✔"), verb, a.p.tag(r.Provider), a.p.bold(r.Label), a.p.dim(r.Email), a.p.dim("("+tier+")"))
	fmt.Fprintln(a.stdout, a.p.dim("   tokens stored in "+a.cfg.StorageDescription()))
	if r.RefreshTokenExpiresAt > 0 {
		at := time.UnixMilli(r.RefreshTokenExpiresAt)
		fmt.Fprintln(a.stdout, a.p.dim(fmt.Sprintf("   login valid until %s (%s)", core.FormatLocal(at), core.FormatRelative(time.Until(at)))))
	}
}

func resolveAiuHookBinary() string {
	if env := strings.TrimSpace(os.Getenv("AIU_HOOK_BIN")); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	exe, _ := os.Executable()
	return resolveAiuHookBinaryFrom(home, exe, func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && !st.IsDir()
	})
}

// resolveAiuHookBinaryFrom selects an upgrade-stable path to `aiu` for external
// CLI hooks, avoiding versioned Homebrew Cellar paths that break on upgrade.
func resolveAiuHookBinaryFrom(home, exe string, exists func(string) bool) string {
	if home != "" {
		if exists(filepath.Join(home, ".local", "bin", "aiu")) {
			return "~/.local/bin/aiu"
		}
	}
	// If running from a versioned Homebrew Cellar bundle (e.g.
	// /opt/homebrew/Cellar/aiu/0.4.0/AIU.app/Contents/MacOS/aiu), prefer the
	// stable Homebrew bin or opt symlink instead.
	if exe != "" {
		norm := filepath.ToSlash(exe)
		if idx := strings.Index(norm, "/Cellar/aiu/"); idx > 0 {
			prefix := norm[:idx]
			for _, candidate := range []string{
				filepath.FromSlash(prefix + "/bin/aiu"),
				filepath.FromSlash(prefix + "/opt/aiu/AIU.app/Contents/MacOS/aiu"),
			} {
				if exists(candidate) {
					return candidate
				}
			}
		}
	}
	for _, candidate := range []string{
		"/opt/homebrew/bin/aiu",
		"/usr/local/bin/aiu",
	} {
		if exists(candidate) {
			return candidate
		}
	}
	if home != "" {
		appBin := filepath.Join(home, "Applications", "AIU.app", "Contents", "MacOS", "aiu")
		if exists(appBin) {
			return "~/Applications/AIU.app/Contents/MacOS/aiu"
		}
	}
	if exists("/Applications/AIU.app/Contents/MacOS/aiu") {
		return "/Applications/AIU.app/Contents/MacOS/aiu"
	}
	if exe != "" {
		return exe
	}
	return "aiu"
}

func (a *app) add(ctx context.Context) error {
	if a.provider() == core.Antigravity {
		return a.addAgy()
	}
	var s *core.SavedAccount
	var err error
	if a.provider() == core.Codex {
		s, err = a.cfg.CaptureCodex(ctx, a.opts.label)
	} else {
		s, err = a.cfg.CaptureClaudeCode(ctx, a.opts.label)
	}
	if err != nil {
		return err
	}
	a.reportSaved(s)
	return nil
}

func (a *app) addAgy() error {
	bin := resolveAiuHookBinary()
	st, err := a.cfg.SetupAgyStatusLine(bin)
	if err != nil {
		return err
	}
	if st.CustomExisting {
		fmt.Fprintf(a.stdout, "%s %s already has a custom statusLine command (%s) — left untouched\n",
			a.p.yellow("!"), st.SettingsPath, st.ExistingCommand)
		fmt.Fprintln(a.stdout, a.p.dim("   add these two lines to your statusLine script to feed Antigravity usage into aiu:"))
		for _, line := range strings.Split(core.AgyHookSnippet(bin), "\n") {
			fmt.Fprintln(a.stdout, "   "+a.p.cyan(line))
		}
		return fmt.Errorf("Antigravity CLI already has a custom statusLine command (%s) — left untouched; pipe stdin into `aiu hook agy` from your script", st.ExistingCommand)
	}
	verb := "enabled"
	if st.AlreadyConfigured {
		verb = "already enabled"
	}
	fmt.Fprintf(a.stdout, "%s %s %s statusLine hook in %s\n", a.p.green("✔"), verb, a.p.tag(core.Antigravity), a.p.dim(st.SettingsPath))
	fmt.Fprintln(a.stdout, a.p.dim("   command: "+st.Command+" (stack_with_default: true)"))
	fmt.Fprintln(a.stdout, a.p.dim("   no credentials stored — usage appears automatically the next time `agy` runs"))
	return nil
}

func (a *app) hook(context.Context) error {
	target := a.opts.provider
	if target == "" && len(a.opts.args) > 0 {
		if p, ok := core.ParseProvider(strings.ToLower(a.opts.args[0])); ok {
			target = p
		}
	}
	if target != core.Antigravity {
		return errors.New("usage: aiu hook agy [--label NAME]  (reads Antigravity CLI statusLine JSON from stdin)")
	}
	data, err := io.ReadAll(io.LimitReader(a.stdin, 1<<20))
	if err != nil {
		return err
	}
	_, err = a.cfg.IngestAgyStatusLine(data, a.opts.label)
	return err
}

func (a *app) login(ctx context.Context) error {
	if len(a.opts.args) > 1 {
		return errors.New("usage: aiu login [email|label] [--codex] [--no-open]")
	}
	selector := ""
	if len(a.opts.args) == 1 {
		selector = a.opts.args[0]
	}
	pr := a.provider()
	if selector != "" {
		_, entry, err := a.cfg.FindAccount(selector, a.opts.provider)
		if err != nil {
			return err
		}
		pr = entry.Provider
	}
	if pr == core.Codex && a.opts.readOnly {
		fmt.Fprintln(a.stdout, a.p.dim("--readonly applies to Claude logins only"))
	}
	s, err := a.cfg.BeginLogin(core.LoginOptions{Provider: pr, Selector: selector, ReadOnly: a.opts.readOnly, Manual: a.opts.manual, UseConsole: a.opts.console})
	if err != nil {
		return err
	}
	defer s.Cancel()
	if pr == core.Claude && !a.opts.readOnly {
		fmt.Fprintln(a.stdout, a.p.dim("signing in with full Claude Code scopes so `switch` works; --readonly mints a usage-only token"))
	}
	var code string
	if s.Manual {
		fmt.Fprintf(a.stdout, "open this URL in a browser signed in to the account you want to add:\n\n  %s\n\n", a.p.cyan(s.AuthorizeURL))
		if !a.opts.noOpen {
			a.cfg.OpenBrowser(s.AuthorizeURL)
		}
		fmt.Fprint(a.stdout, "paste the authorization code shown after approving: ")
		line, err := readLoginCode(ctx, os.Stdin)
		if err != nil {
			return err
		}
		if code = strings.TrimSpace(line); code == "" {
			return errors.New("no code entered")
		}
	} else {
		fmt.Fprintf(a.stdout, "opening browser for %s login… %s\n", pr.Name(), a.p.dim("(a private window helps when adding a second account)"))
		fmt.Fprintln(a.stdout, a.p.dim("if no browser opens, use this URL:\n  "+s.AuthorizeURL))
		if !a.opts.noOpen {
			a.cfg.OpenBrowser(s.AuthorizeURL)
		}
		if code, err = s.WaitForCode(ctx); err != nil {
			return err
		}
	}
	saved, err := a.cfg.CompleteLogin(ctx, s, code, a.opts.label)
	if err != nil {
		return err
	}
	a.reportSaved(saved)
	fmt.Fprintln(a.stdout, a.p.dim("   this login is its own grant — "+pr.Client()+"'s own login and logout do not touch it"))
	liveEmail := a.cfg.ClaudeCachedEmail()
	if pr == core.Codex {
		if live := a.cfg.ReadCodexAuth(); live != nil {
			liveEmail = live.LiveEmail()
		}
	}
	if liveEmail == saved.Record.Email {
		fmt.Fprintln(a.stderr, a.p.yellow(fmt.Sprintf("warn: %s is signed in as %s too. Authorizing the same account twice may invalidate the older token — prefer `aiu add` for the account %s uses now.",
			pr.Client(), saved.Record.Email, pr.Client())))
	}
	return nil
}

func (a *app) list(ctx context.Context) error {
	idx, err := a.cfg.LoadIndex()
	if err != nil {
		return err
	}
	if len(idx.Accounts) == 0 {
		fmt.Fprintln(a.stdout, a.p.dim("no accounts tracked — run `aiu add` or `aiu login`"))
		return nil
	}
	records, err := a.cfg.LoadRecords(idx)
	if err != nil {
		return err
	}
	live, err := a.cfg.DescribeLive(ctx, false)
	if err != nil {
		return err
	}
	lw := 8
	tw := 9
	for _, r := range records {
		lw = max(lw, core.DisplayWidth(r.Label))
		tw = max(tw, core.DisplayWidth(r.Provider.Glyph()+" "+r.Provider.Name()))
	}
	now := time.Now()
	for _, r := range records {
		active := ""
		if m := live[r.Provider]; m != nil && m.Is(r) {
			active = a.p.green(" active")
		}
		var state string
		if r.Provider.IsPassive() {
			parts := []string{"passive", "read-only"}
			if t := core.TierLabel(r); t != "" {
				parts = append(parts, t)
			}
			parts = append(parts, "via agy statusLine")
			state = a.p.dim(strings.Join(parts, " · "))
		} else if r.Missing {
			state = a.p.red("token missing")
		} else {
			parts := []string{"refreshable"}
			if r.RefreshToken == "" {
				parts[0] = "no refresh token"
			}
			if r.IsExpired(now, 0) {
				parts = append(parts, "access "+a.p.yellow("expired"))
			} else {
				parts = append(parts, "access valid "+core.FormatRelative(time.Until(time.UnixMilli(r.ExpiresAt))))
			}
			if r.RefreshTokenExpiresAt > 0 {
				parts = append(parts, "login "+core.FormatRelative(time.Until(time.UnixMilli(r.RefreshTokenExpiresAt)))+" left")
			}
			if r.IsReadOnly() {
				parts = append(parts, "read-only")
			}
			if name := core.DistinctOrgName(r); name != "" {
				parts = append(parts, name)
			}
			if r.Source != "" {
				parts = append(parts, "via "+r.Source)
			}
			state = a.p.dim(strings.Join(parts, " · "))
		}
		fmt.Fprintf(a.stdout, "%s %s %s %s%s\n", padVisible(a.p.tag(r.Provider), tw), a.p.bold(padVisible(r.Label, lw)), padVisible(r.Email, 28), state, active)
	}
	return nil
}

func (a *app) migrateKeychain(context.Context) error {
	if len(a.opts.args) != 0 {
		return errors.New("usage: aiu migrate-keychain")
	}
	count, err := a.cfg.MigrateKeychainVault()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "AIU Keychain vault ready: %d account(s) in one item\n", count)
	return nil
}

func (a *app) remove(context.Context) error {
	if len(a.opts.args) == 0 {
		return errors.New("usage: aiu remove <email|label> [--codex]")
	}
	e, err := a.cfg.RemoveAccount(a.opts.args[0], a.opts.provider)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s removed %s %s\n", a.p.green("✔"), a.p.tag(e.Provider), e.Email)
	return nil
}

func (a *app) sync(ctx context.Context) error {
	idx, err := a.cfg.LoadIndex()
	if err != nil {
		return err
	}
	records, err := a.cfg.LoadRecords(idx)
	if err != nil {
		return err
	}
	tracked := func(rs []*core.Record, p core.Provider, m core.LiveMatch) bool {
		for _, r := range rs {
			if r.Provider == p && !r.Missing && m.Is(r) {
				return true
			}
		}
		return false
	}
	if a.opts.provider == "" || a.opts.provider == core.Claude {
		live, m, synced := a.cfg.SyncClaude(ctx, records, true)
		switch {
		case live == nil:
			fmt.Fprintln(a.stdout, a.p.dim("no Claude Code login found"))
		case !m.Verified:
			fmt.Fprintln(a.stdout, a.p.yellow("! a Claude Code login exists but could not be identified — run `claude` once so its token refreshes, then retry"))
		case tracked(synced, core.Claude, m):
			fmt.Fprintf(a.stdout, "%s %s %s is up to date\n", a.p.green("✔"), a.p.tag(core.Claude), m.Email)
		default:
			fmt.Fprintf(a.stdout, "%s %s is signed in to Claude Code but not tracked — run `aiu add`\n", a.p.yellow("!"), m.Email)
		}
	}
	if a.opts.provider == "" || a.opts.provider == core.Codex {
		live, m, synced := a.cfg.SyncCodex(records, true)
		switch {
		case live == nil:
			fmt.Fprintln(a.stdout, a.p.dim("no Codex login found"))
		case m.Email == "":
			fmt.Fprintln(a.stdout, a.p.yellow("! a Codex login exists but its id_token carries no email — run `codex login` again"))
		case tracked(synced, core.Codex, m):
			fmt.Fprintf(a.stdout, "%s %s %s is up to date\n", a.p.green("✔"), a.p.tag(core.Codex), m.Email)
		default:
			fmt.Fprintf(a.stdout, "%s %s is signed in to Codex but not tracked — run `aiu add --codex`\n", a.p.yellow("!"), m.Email)
		}
	}
	if a.opts.provider == core.Antigravity || (a.opts.provider == "" && (a.cfg.AgyHookConfigured().Configured || a.cfg.AgyActiveEmail() != "")) {
		st := a.cfg.AgyHookConfigured()
		email := a.cfg.AgyActiveEmail()
		switch {
		case email != "" && st.Configured:
			fmt.Fprintf(a.stdout, "%s %s %s is tracked via agy statusLine\n", a.p.green("✔"), a.p.tag(core.Antigravity), email)
		case st.Configured:
			fmt.Fprintf(a.stdout, "%s %s statusLine hook is enabled; waiting for next `agy` session\n", a.p.green("✔"), a.p.tag(core.Antigravity))
		case email != "":
			fmt.Fprintf(a.stdout, "%s %s last saw %s (statusLine hook not detected in %s)\n", a.p.yellow("!"), a.p.tag(core.Antigravity), email, st.SettingsPath)
		default:
			fmt.Fprintln(a.stdout, a.p.dim("Antigravity CLI statusLine hook not configured — run `aiu add --agy`"))
		}
	}
	return nil
}

func (a *app) whoami(ctx context.Context) error {
	live, err := a.cfg.DescribeLive(ctx, true)
	if err != nil {
		return err
	}
	tw := 9
	for _, pr := range core.Providers {
		if a.opts.provider != "" && pr != a.opts.provider {
			continue
		}
		tw = max(tw, core.DisplayWidth(pr.Glyph()+" "+pr.Name()))
	}
	for _, pr := range core.Providers {
		if a.opts.provider != "" && pr != a.opts.provider {
			continue
		}
		prefix := padVisible(a.p.tag(pr), tw) + " "
		switch m := live[pr]; {
		case m == nil:
			if pr == core.Antigravity {
				fmt.Fprintln(a.stdout, prefix+a.p.dim("no agy statusLine reading yet"))
			} else {
				fmt.Fprintln(a.stdout, prefix+a.p.dim("not signed in to "+pr.Client()))
			}
		case m.Email == "":
			fmt.Fprintln(a.stdout, prefix+a.p.dim("unknown (token expired and no cached email)"))
		case m.Verified:
			if pr == core.Antigravity {
				fmt.Fprintln(a.stdout, prefix+m.Email+" "+a.p.dim("(via agy statusLine)"))
			} else {
				fmt.Fprintln(a.stdout, prefix+m.Email)
			}
		default:
			fmt.Fprintln(a.stdout, prefix+m.Email+" "+a.p.dim("(unverified — from .claude.json)"))
		}
	}
	return nil
}

func (a *app) switchTo(ctx context.Context) error {
	if len(a.opts.args) == 0 {
		return errors.New("usage: aiu switch <email|label> [--codex]")
	}
	// A Codex daemon restart that has started keeps running after Ctrl-C;
	// stopping it midway would leave Codex refusing new sessions.
	finished, reported := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(reported)
		select {
		case <-ctx.Done():
			fmt.Fprintln(a.stderr, a.p.yellow("Interrupted. Finishing any Codex daemon restart already under way so new Codex sessions keep working; press Ctrl-C again to exit (the restart still completes)."))
		case <-finished:
		}
	}()
	res, err := a.cfg.SwitchAccount(ctx, a.opts.args[0], a.opts.provider)
	close(finished)
	<-reported
	if err != nil {
		return err
	}
	if res.UntrackedReplaced != "" {
		fmt.Fprintln(a.stderr, a.p.yellow("warn: the previous login ("+res.UntrackedReplaced+") was not tracked; it has been replaced. Run `aiu add` before switching next time to keep it."))
	}
	if res.CodexDaemon != nil && res.CodexDaemon.Warning {
		fmt.Fprintln(a.stderr, a.p.yellow("warn: "+res.CodexDaemon.Message))
	}
	if a.opts.json {
		return json.NewEncoder(a.stdout).Encode(res)
	}
	client := res.Entry.Provider.Client()
	if res.Entry.Provider == core.Codex {
		client += " saved login"
	}
	if res.AlreadyActive {
		fmt.Fprintf(a.stdout, "%s %s already uses %s\n", a.p.green("✔"), client, res.Entry.Email)
		if res.Entry.Provider == core.Codex {
			a.codexSwitchGuidance(res)
		}
		return nil
	}
	note := ""
	if !res.UpdatedGlobal {
		note = a.p.dim(" (account cache in .claude.json not updated)")
	}
	fmt.Fprintf(a.stdout, "%s %s now uses %s %s %s%s\n", a.p.green("✔"), client, a.p.tag(res.Entry.Provider), a.p.bold(res.Entry.Label), a.p.dim(res.Entry.Email), note)
	if res.Entry.Provider == core.Claude {
		fmt.Fprintln(a.stdout, a.p.dim("   running claude sessions pick it up within about 30s; new sessions use it immediately"))
	} else {
		a.codexSwitchGuidance(res)
	}
	return nil
}

func (a *app) codexSwitchGuidance(res *core.SwitchResult) {
	if res.CodexDaemon != nil && !res.CodexDaemon.Warning {
		fmt.Fprintln(a.stdout, a.p.dim("   "+res.CodexDaemon.Message))
	}
}

// menuBar opens the installed AIU.app, which draws the panel and calls this binary.
func (a *app) menuBar(context.Context) error {
	if err := openMenuBar(); err != nil {
		return err
	}
	return nil
}

func (a *app) help() {
	b := a.p.bold
	fmt.Fprintf(a.stdout, `%s — rate limits and reset times across Claude, ChatGPT (Codex), and Antigravity (agy) accounts

%s
  aiu [status] [--json] [--sort 5h|7d] [--no-sync]    all tracked accounts (default)
  aiu watch [--interval 60]                           live view (requests: ≤1 per %.0f min per account)
  aiu add [--label NAME]                              track the account Claude Code is signed in as
  aiu login [--label NAME] [--readonly] [--manual]    browser sign-in for another account
  aiu login <email|label>                             relink a tracked account with its browser session
  aiu switch <email|label>                            point Claude Code (or Codex) at a tracked account
  aiu list | remove <email|label> | sync | whoami
  aiu link [install|remove|status]                    add or drop the ~/.local/bin/aiu shortcut
  aiu migrate-keychain                                copy old AIU tokens into one Keychain item
  aiu keychain [allow claude|aiu]                     which Keychain items macOS would ask about
  aiu update [--force]                                is there a newer release? (never installs)
  aiu gui | menubar                                    open the bundled desktop app

%s
  Every command takes --codex (or --provider codex) to act on Codex instead:
  aiu add --codex                                     track the account Codex is signed in as
  aiu login --codex [--label NAME]                    sign in to another ChatGPT account
  aiu switch codex:NAME                               a claude:/codex:/agy: prefix also disambiguates names
  aiu resets codex:NAME [--json]                      view banked reset balance and expiration details
  aiu reset codex:NAME --yes [--credit-id ID]         use one banked reset (moves the weekly reset date)
  aiu auto-reset codex:NAME --enabled true|false [--threshold N]
  aiu auto-reset codex:NAME --threshold N             set remaining %% (0-99, default 1); preserves enabled state
  Auto reset is off by default. 0%% means fully exhausted; the threshold is saved per account.
  Reset checks run during desktop, watch, or status polling. --request-id UUID retries the same reset.

%s
  Antigravity is tracked passively via agy's official statusLine hook (no OAuth tokens stored or switched):
  aiu add --agy                                       enable the statusLine hook in ~/.gemini/antigravity-cli/settings.json
  aiu hook agy                                        statusLine stdin collector (invoked automatically by agy)
  aiu --agy | aiu remove agy:NAME                     filter or remove a tracked Antigravity account

%s
  index    %s
  tokens   %s
  env      AIU_STORE=file  AIU_CONFIG_DIR  CLAUDE_CONFIG_DIR  CODEX_HOME  AIU_AGY_SETTINGS  NO_COLOR
`, b("aiu"), b("usage"), core.MinFetchSpacing.Minutes(), b("codex"), b("antigravity"), b("storage"), a.cfg.IndexPath(), a.cfg.StorageDescription())
}
