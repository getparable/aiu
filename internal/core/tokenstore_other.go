//go:build !windows

package core

func readTokenStore(c *Config, out any) (bool, error) { return readJSONFile(c.fileStore(), out) }
func writeTokenStore(c *Config, v any) error          { return writePrivateJSON(c.fileStore(), v) }
