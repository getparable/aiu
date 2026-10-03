package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// withFileLock serializes read/modify/write operations between AIU processes.
var errLockTimeout = errors.New("credential store is locked by another process")

func withFileLock(path string, fn func() error) error {
	return withFileLockTimeout(path, lockWait, fn)
}

func withFileLockTimeout(path string, wait time.Duration, fn func() error) error {
	return withFileLockContext(context.Background(), path, wait, fn)
}

func withFileLockContext(ctx context.Context, path string, wait time.Duration, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if waitCtx.Err() != nil {
			return errLockTimeout
		}
		err = tryPlatformLock(f)
		if err == nil {
			defer unlockPlatformLock(f)
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		}
		if !isLockContended(err) {
			return err
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
}
