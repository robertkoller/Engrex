package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	MCPEnabled bool `json:"mcp_enabled"`
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".engrex", "config.json"), nil
}

func Load() (Config, error) {
	var configuration Config

	path, err := Path()
	if err != nil {
		return configuration, err
	}

	contents, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return configuration, nil
	}
	if err != nil {
		return configuration, err
	}
	if err := json.Unmarshal(contents, &configuration); err != nil {
		return configuration, err
	}
	return configuration, nil
}

func Save(configuration Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	contents, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(contents, '\n'), 0600)
}
