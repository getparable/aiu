package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/getparable/aiu/internal/core"
)

type frontendEvent struct {
	Version      int            `json:"version"`
	Event        string         `json:"event"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Phase        string         `json:"phase,omitempty"`
	Message      string         `json:"message,omitempty"`
	OK           bool           `json:"ok"`
	Cancelled    bool           `json:"cancelled"`
	Accounts     []jsonAccount  `json:"accounts"`
	Error        *frontendError `json:"error,omitempty"`
}
type frontendError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var frontendCapabilities = []string{"status", "add", "login", "switch", "remove", "sync", "cancel"}

func runFrontend(opts *options, cfg *core.Config, in io.Reader, out io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enc := json.NewEncoder(out)
	var outputMu sync.Mutex
	emit := func(e frontendEvent) {
		outputMu.Lock()
		defer outputMu.Unlock()
		e.Version = 1
		if enc.Encode(e) != nil {
			cancel()
		}
	}
	emit(frontendEvent{Event: "hello", Capabilities: frontendCapabilities})
	if opts.contractVersion != "" && opts.contractVersion != "1" {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "unsupported_version", Message: "frontend contract version is unsupported"}})
		return 2
	}
	// Informational flags must never start the requested mutation. Keep these
	// responses inside the JSONL contract instead of printing terminal help.
	if opts.version {
		emit(frontendEvent{Event: "result", OK: true, Message: "aiu " + core.Version})
		return 0
	}
	if opts.help {
		emit(frontendEvent{Event: "result", OK: true, Message: "aiu frontend <status|add|login|switch|remove|sync> --contract-version 1; keep stdin open and send {\"cancel\":true} to cancel"})
		return 0
	}
	if len(opts.args) == 0 {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_command", Message: "frontend needs a command"}})
		return 2
	}
	command := opts.args[0]
	valid := false
	for _, capability := range frontendCapabilities {
		if capability == command && command != "cancel" {
			valid = true
		}
	}
	if !valid {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_command", Message: "unknown frontend command"}})
		return 2
	}
	if (command == "switch" || command == "remove") && len(opts.args) != 2 {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_command", Message: command + " needs one selector"}})
		return 2
	}
	if (command == "status" || command == "add" || command == "login" || command == "sync") && len(opts.args) != 1 {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_command", Message: command + " does not accept a selector"}})
		return 2
	}
	if opts.manual {
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_input", Message: "manual login is available through the terminal command `aiu login --manual`"}})
		return 2
	}
	// Provider requests may emit warnings concurrently. Keep every stdout write
	// inside the serialized encoder, and do not change the caller's Config hooks.
	config := *cfg
	cfg = &config
	cfg.Warn = func(message string) {
		emit(frontendEvent{Event: "progress", Phase: "working", Message: core.Redact(message)})
	}
	cfg.Info = func(string) {}
	inputErr := make(chan error, 1)
	go watchFrontendInput(in, cancel, inputErr)
	emit(frontendEvent{Event: "progress", Phase: "working", Message: command})
	result, err := frontendCommand(ctx, cfg, opts, command, emit)
	var accounts []jsonAccount
	if err == nil {
		var snapshotErr error
		accounts, snapshotErr = frontendAccounts(ctx, cfg, opts, command == "status")
		if command == "status" {
			err = snapshotErr
		} else if snapshotErr != nil {
			// The mutation already committed. Reporting cancellation here could
			// encourage repeating a login whose credentials were successfully saved.
			result += "; snapshot unavailable"
		}
	}
	if err != nil {
		select {
		case inputFailure := <-inputErr:
			emit(frontendEvent{Event: "result", Error: &frontendError{Code: "invalid_input", Message: inputFailure.Error()}})
			return 2
		default:
		}
		if errors.Is(err, context.Canceled) {
			emit(frontendEvent{Event: "result", Cancelled: true, Error: &frontendError{Code: "cancelled", Message: "frontend command cancelled"}})
			return 130
		}
		emit(frontendEvent{Event: "result", Error: &frontendError{Code: "operation_failed", Message: core.Redact(err.Error())}})
		return 1
	}
	emit(frontendEvent{Event: "result", OK: true, Message: result, Accounts: accounts})
	return 0
}

func watchFrontendInput(in io.Reader, cancel func(), failures chan<- error) {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 1024), 8192)
	if s.Scan() {
		var msg struct {
			Cancel  bool `json:"cancel"`
			Version *int `json:"version"`
		}
		if err := json.Unmarshal(s.Bytes(), &msg); err != nil {
			failures <- errors.New("malformed frontend input")
			cancel()
			return
		}
		if msg.Version != nil && *msg.Version != 1 {
			failures <- errors.New("unsupported frontend protocol version")
			cancel()
			return
		}
		if msg.Cancel {
			cancel()
			return
		}
		failures <- errors.New("unknown frontend control message")
		cancel()
		return
	}
	if s.Err() != nil {
		failures <- errors.New("frontend input exceeds its size limit or could not be read")
	}
	cancel()
}

func frontendCommand(ctx context.Context, cfg *core.Config, opts *options, command string, emit func(frontendEvent)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	selector := ""
	if len(opts.args) > 1 {
		selector = opts.args[1]
	}
	switch command {
	case "status":
		return "status complete", ctx.Err()
	case "add":
		var err error
		if opts.provider == core.Codex {
			_, err = cfg.CaptureCodex(ctx, opts.label)
		} else {
			_, err = cfg.CaptureClaudeCode(ctx, opts.label)
		}
		if err != nil {
			return "", err
		}
		return "account added", nil
	case "login":
		s, err := cfg.BeginLogin(core.LoginOptions{Provider: frontendProvider(opts), ReadOnly: opts.readOnly, Manual: opts.manual, UseConsole: opts.console})
		if err != nil {
			return "", err
		}
		defer s.Cancel()
		if !opts.noOpen && !cfg.OpenBrowser(s.AuthorizeURL) {
			s.Cancel()
			return "", errors.New("could not open the browser; retry with `aiu login --no-open` and open the login URL from the terminal")
		}
		emit(frontendEvent{Event: "progress", Phase: "waiting", Message: "waiting for browser authorization"})
		code, err := s.WaitForCode(ctx)
		if err != nil {
			return "", err
		}
		emit(frontendEvent{Event: "progress", Phase: "exchanging", Message: "exchanging authorization"})
		if _, err = cfg.CompleteLogin(ctx, s, code, opts.label); err != nil {
			return "", err
		}
		return "login complete", nil
	case "switch":
		if selector == "" {
			return "", errors.New("frontend switch needs an account selector")
		}
		_, err := cfg.SwitchAccount(ctx, selector, opts.provider)
		return "account switched", err
	case "remove":
		if selector == "" {
			return "", errors.New("frontend remove needs an account selector")
		}
		_, err := cfg.RemoveAccount(selector, opts.provider)
		return "account removed", err
	case "sync":
		return "sync complete", frontendSync(ctx, cfg, opts)
	default:
		return "", errors.New("unknown frontend command")
	}
}

func frontendSync(ctx context.Context, cfg *core.Config, opts *options) error {
	idx, e := cfg.LoadIndex()
	if e != nil {
		return e
	}
	rs, e := cfg.LoadRecords(idx)
	if e != nil {
		return e
	}
	if opts.provider != core.Codex {
		_, _, rs = cfg.SyncClaude(ctx, rs, true)
	}
	if opts.provider != core.Claude {
		_, _, _ = cfg.SyncCodex(rs, true)
	}
	return ctx.Err()
}
func frontendProviders(opts *options) []core.Provider {
	if opts.provider != "" {
		return []core.Provider{opts.provider}
	}
	return nil
}
func frontendProvider(opts *options) core.Provider {
	if opts.provider != "" {
		return opts.provider
	}
	return core.Claude
}
func frontendAccounts(ctx context.Context, cfg *core.Config, opts *options, status bool) ([]jsonAccount, error) {
	s, e := cfg.Collect(ctx, core.CollectOptions{Providers: frontendProviders(opts), NoSync: !status || opts.noSync})
	if e != nil {
		return nil, e
	}
	return toJSON(s, time.Now()), nil
}
