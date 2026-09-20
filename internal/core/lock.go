package core

import (
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(wait)
	for {
		err = tryPlatformLock(f)
		if err == nil {
			defer unlockPlatformLock(f)
			return fn()
		}
		if !isLockContended(err) {
			return err
		}
		if time.Now().After(deadline) {
			return errLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}
