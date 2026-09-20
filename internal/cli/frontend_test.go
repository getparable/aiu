package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/getparable/aiu/internal/core"
)

func TestFrontendInformationalFlagsDoNotRemoveAccount(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "--version", "-v", "-V"} {
		t.Run(flag, func(t *testing.T) {
			cfg := core.DefaultConfig()
			cfg.Dir = t.TempDir()
			cfg.UseKeychain, cfg.UseDPAPI = false, false
			index := []byte(`{"version":1,"accounts":[{"provider":"claude","email":"fixture@example.test","org":"fixture-org"}]}`)
			path := filepath.Join(cfg.Dir, "accounts.json")
			if err := os.WriteFile(path, index, 0600); err != nil {
				t.Fatal(err)
			}
			opts, err := parse([]string{"frontend", "remove", "claude:fixture@example.test#fixture-org", flag})
			if err != nil {
				t.Fatal(err)
			}
			in, parent := io.Pipe()
			defer func() { _ = in.Close(); _ = parent.Close() }()
			var out bytes.Buffer
			code := runFrontend(opts, cfg, in, &out)
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, index) {
				t.Fatalf("informational flag mutated the account index: %s, %v", got, err)
			}
			events := decodeFrontend(t, out.Bytes())
			if code != 0 || len(events) != 2 || events[0].Event != "hello" || events[1].Event != "result" || !events[1].OK || events[1].Message == "" {
				t.Fatalf("exit=%d events=%+v", code, events)
			}
		})
	}
}

func decodeFrontend(t *testing.T, b []byte) []frontendEvent {
	t.Helper()
	var out []frontendEvent
	for s := bufio.NewScanner(bytes.NewReader(b)); s.Scan(); {
		var e frontendEvent
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestFrontendInvalidCommandIsStructured(t *testing.T) {
	var out bytes.Buffer
	code := runFrontend(&options{args: []string{"bogus"}}, core.DefaultConfig(), bytes.NewReader(nil), &out)
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	events := decodeFrontend(t, out.Bytes())
	if len(events) != 2 || events[0].Event != "hello" || events[0].Version != 1 || events[1].Error == nil || events[1].Error.Code != "invalid_command" {
		t.Fatalf("events=%+v", events)
	}
}

func TestFrontendLoginCancelIsTerminal(t *testing.T) {
	var out bytes.Buffer
	code := runFrontend(&options{args: []string{"login"}, noOpen: true}, core.DefaultConfig(), bytes.NewBufferString(`{"cancel":true}`+"\n"), &out)
	if code != 130 {
		t.Fatalf("exit=%d output=%s", code, out.String())
	}
	events := decodeFrontend(t, out.Bytes())
	if len(events) < 3 || events[0].Event != "hello" || events[len(events)-1].Event != "result" || !events[len(events)-1].Cancelled {
		t.Fatalf("events=%+v", events)
	}
}
