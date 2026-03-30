package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type DefaultConfig struct {
	Mode     string `toml:"mode"`
	Duration string `toml:"duration"`
	Until    string `toml:"until"`
	Await    string `toml:"await"`
	Every    string `toml:"every"`
	Hold     string `toml:"hold"`
	What     string `toml:"what"`
}

type Config struct {
	Default DefaultConfig `toml:"default"`
}

func configPath() string {
	if p := os.Getenv("BLOCK_SLEEP_CONFIG_PATH"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "block-sleep", "config.toml")
}

func loadConfig(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = configPath()
	}
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return nil, fmt.Errorf("config file not found: %s", path)
			}
			return nil, nil
		}
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Apply defaults for unset fields
	if cfg.Default.Mode == "" {
		cfg.Default.Mode = "for"
	}
	if cfg.Default.Duration == "" && cfg.Default.Mode == "for" {
		cfg.Default.Duration = "1h"
	}
	if cfg.Default.What == "" {
		cfg.Default.What = defaultWhat
	}

	return &cfg, nil
}

