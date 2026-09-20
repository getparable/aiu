//go:build windows

package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"unsafe"
)

func dpapiStorePath(c *Config) string { return filepath.Join(c.Dir, "tokens.dpapi") }

func readTokenStore(c *Config, out any) (bool, error) {
	if !c.UseDPAPI {
		return readJSONFile(c.fileStore(), out)
	}
	if _, err := os.Stat(c.fileStore()); err == nil {
		return false, errors.New("plaintext token store exists at " + c.fileStore() + "; set AIU_STORE=file to use it")
	}
	b, err := os.ReadFile(dpapiStorePath(c))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	plain, err := dpapiUnprotect(b)
	if err != nil {
		return false, fmt.Errorf("decrypting token store: %w", err)
	}
	if err := json.Unmarshal(plain, out); err != nil {
		return false, fmt.Errorf("stored token data is corrupt: %w", err)
	}
	return true, nil
}
func writeTokenStore(c *Config, v any) error {
	if !c.UseDPAPI {
		return writePrivateJSON(c.fileStore(), v)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	enc, err := dpapiProtect(data)
	if err != nil {
		return err
	}
	return writePrivateFile(dpapiStorePath(c), enc, true)
}

func dpapiProtect(data []byte) ([]byte, error) {
	var p *byte
	if len(data) > 0 {
		p = &data[0]
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: p}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
func dpapiUnprotect(data []byte) ([]byte, error) {
	var p *byte
	if len(data) > 0 {
		p = &data[0]
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: p}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
