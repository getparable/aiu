//go:build windows

package core

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func tryPlatformLock(f *os.File) error {
	var ov windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov)
}
func unlockPlatformLock(f *os.File) {
	var ov windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ov)
}
func isLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)
}
