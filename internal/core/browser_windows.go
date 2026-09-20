//go:build windows

package core

import "golang.org/x/sys/windows"

// Invoke the system URL association directly; never interpolate a shell command.
func openBrowser(rawURL string) bool {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(rawURL)
	if err != nil {
		return false
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL) == nil
}
