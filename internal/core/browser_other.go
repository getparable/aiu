//go:build !darwin && !windows

package core

func isDarwin() bool { return false }
