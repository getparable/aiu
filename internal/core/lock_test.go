package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockSubprocessContention(t *testing.T) {
	if os.Getenv("AIU_LOCK_HELPER") == "1" {
		if err := withFileLock(os.Getenv("AIU_LOCK_PATH"), func() error {
			fmt.Fprintln(os.Stdout, "locked")
			_, err := io.Copy(io.Discard, os.Stdin)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "contention.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockSubprocessContention$")
	cmd.Env = append(os.Environ(), "AIU_LOCK_HELPER=1", "AIU_LOCK_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cancel(); cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire lock: %q %v", line, err)
	}
	begin := time.Now()
	err = withFileLockTimeout(path, 100*time.Millisecond, func() error {
		t.Error("entered critical section while another process held the lock")
		return nil
	})
	if elapsed := time.Since(begin); !errors.Is(err, errLockTimeout) || elapsed > 5*time.Second {
		t.Fatalf("bounded wait: %v after %v", err, elapsed)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := withFileLockTimeout(path, time.Second, func() error { return nil }); err != nil {
		t.Fatalf("lock not released on process exit: %v", err)
	}
}
