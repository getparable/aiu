package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestManualLoginReadCanBeCanceled(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := readLoginCode(ctx, r); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("manual login is still waiting for console input")
	}
}

func TestManualLoginInput(t *testing.T) {
	for _, input := range []string{"synthetic-code\r\n", "synthetic-code\n", "synthetic-code"} {
		if got, err := readLoginCode(context.Background(), strings.NewReader(input)); got != "synthetic-code" || err != nil {
			t.Fatalf("read: %q, %v", got, err)
		}
	}
	if _, err := readLoginCode(context.Background(), strings.NewReader(strings.Repeat("x", 9000))); err == nil {
		t.Fatal("unbounded authorization code accepted")
	}
}
