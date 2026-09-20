//go:build windows

package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type linkOwner struct {
	Binary string `json:"binary"`
	Path   string `json:"path"`
	Hash   string `json:"sha256"`
}

func linkDir() string                        { h, _ := os.UserHomeDir(); return filepath.Join(h, ".local", "bin") }
func linkPath() string                       { return filepath.Join(linkDir(), "aiu.exe") }
func ownerPath() string                      { return linkPath() + ".owner.json" }
func resolveBinary(e string) (string, error) { return filepath.Abs(e) }
func readOwner() (linkOwner, error) {
	var o linkOwner
	b, e := os.ReadFile(ownerPath())
	if e != nil {
		return o, e
	}
	e = json.Unmarshal(b, &o)
	return o, e
}
func isOwnedLink(i os.FileInfo) bool {
	if !i.Mode().IsRegular() {
		return false
	}
	o, e := readOwner()
	if e != nil || o.Binary == "" || o.Hash == "" {
		return false
	}
	p, e := filepath.Abs(linkPath())
	if e != nil || !strings.EqualFold(filepath.Clean(o.Path), filepath.Clean(p)) {
		return false
	}
	h, e := fileHash(linkPath())
	return e == nil && strings.EqualFold(h, o.Hash)
}
func linkTarget() string { o, _ := readOwner(); return o.Binary }
func dirOnLoginPath(d string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		p = strings.TrimSpace(strings.Trim(p, `"`))
		if strings.EqualFold(filepath.Clean(p), filepath.Clean(d)) {
			return true
		}
	}
	return false
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func replaceFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func samePath(a, b string) bool {
	x, e1 := filepath.Abs(a)
	y, e2 := filepath.Abs(b)
	return e1 == nil && e2 == nil && strings.EqualFold(filepath.Clean(x), filepath.Clean(y))
}

func installLink() error {
	b, e := currentBinary()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(linkDir(), 0755); e != nil {
		return e
	}
	if i, e := os.Lstat(linkPath()); e == nil {
		if !isOwnedLink(i) {
			return fmt.Errorf("%s exists and is not an aiu installation — remove it yourself first", linkPath())
		}
		if samePath(b, linkPath()) {
			return nil
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	tmp, e := os.CreateTemp(linkDir(), ".aiu-*.tmp")
	if e != nil {
		return e
	}
	n := tmp.Name()
	defer os.Remove(n)
	src, e := os.Open(b)
	if e != nil {
		tmp.Close()
		return e
	}
	_, e = io.Copy(tmp, src)
	src.Close()
	if e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Sync(); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	if e = os.Chmod(n, 0755); e != nil {
		return e
	}
	hash, e := fileHash(n)
	if e != nil {
		return e
	}
	marker, e := os.CreateTemp(linkDir(), ".aiu-owner-*.tmp")
	if e != nil {
		return e
	}
	markerName := marker.Name()
	defer os.Remove(markerName)
	data, _ := json.Marshal(linkOwner{Binary: b, Path: linkPath(), Hash: hash})
	if _, e = marker.Write(data); e != nil {
		marker.Close()
		return e
	}
	if e = marker.Chmod(0600); e != nil {
		marker.Close()
		return e
	}
	if e = marker.Sync(); e != nil {
		marker.Close()
		return e
	}
	if e = marker.Close(); e != nil {
		return e
	}
	if e = replaceFile(n, linkPath()); e != nil {
		return e
	}
	return replaceFile(markerName, ownerPath())
}
func removeLink() error {
	i, e := os.Lstat(linkPath())
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !isOwnedLink(i) {
		return fmt.Errorf("%s is not an aiu installation — leaving it alone", linkPath())
	}
	if e = os.Remove(linkPath()); e != nil {
		return e
	}
	return os.Remove(ownerPath())
}

func sameInstallation(target, binary string) bool {
	if samePath(binary, linkPath()) {
		return true
	}
	currentHash, err := fileHash(binary)
	owner, ownerErr := readOwner()
	return err == nil && ownerErr == nil && currentHash == owner.Hash
}
