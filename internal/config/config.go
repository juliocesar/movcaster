// Package config persists a tiny bit of state between runs, chiefly the last
// device used, so casting without -t keeps working across the TV's port changes.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the persisted state.
type Config struct {
	LastDeviceHost string `json:"last_device_host"` // bare IP, port-independent
	// LastDeviceEngine is the engine that owned the last device. Empty (an old
	// config, or pre-engine state) means "try every engine against that host",
	// which is exactly what the single-engine behavior was.
	LastDeviceEngine string `json:"last_device_engine,omitempty"`
}

func path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "movcaster", "config.json"), nil
}

// Load reads the config, returning a zero Config if none exists.
func Load() Config {
	var c Config
	p, err := path()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// Save writes the config best-effort (errors are ignored by callers).
func Save(c Config) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}
