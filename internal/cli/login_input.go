package cli

import (
	"bufio"
	"context"
	"io"
)

// A console read cannot be interrupted portably. Let the CLI return on Ctrl+C
// (and exit its process) while the single input reader remains blocked. The
// reader belongs to the caller; closing stdin here would affect other commands.
func readLoginCode(ctx context.Context, input io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 1024), 8192)
		if scanner.Scan() {
			done <- result{line: scanner.Text()}
			return
		}
		done <- result{err: scanner.Err()}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-done:
		return res.line, res.err
	}
}
