package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/getparable/aiu/internal/core"
)

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
