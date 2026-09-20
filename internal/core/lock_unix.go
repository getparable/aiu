//go:build !windows

package core

import (
	"errors"
	"os"
	"syscall"
)

func tryPlatformLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func unlockPlatformLock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func isLockContended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
