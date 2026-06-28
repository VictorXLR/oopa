// Package config persists user settings (LM Studio endpoint and chosen
// model) to a small JSON file so they survive restarts. It is deliberately
// tiny: load at startup, save whenever the user changes a setting from the
// TUI or the web settings page.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds the persisted user settings.
type Config struct {
	// BaseURL is the LM Studio OpenAI-compatible base, e.g.
	// "http://127.0.0.1:1234/v1". Empty means "auto-probe defaults".
	BaseURL string `json:"base_url"`
	// Model is the chosen model id. Empty means "auto-pick first chat model".
	Model string `json:"model"`
}

// DefaultPath returns the config file location, honoring OOPA_CONFIG and
// falling back to ~/.oopa-config.json (or the CWD if there is no home).
func DefaultPath() string {
	if v := os.Getenv("OOPA_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".oopa-config.json"
	}
	return filepath.Join(home, ".oopa-config.json")
}

// Load reads the config from path, returning a zero Config if it is missing.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// Save writes the config to path atomically (temp file + rename).
func Save(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".oopa-cfg-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
