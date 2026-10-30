package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Settings are the choices made from the dashboard rather than the command
// line, kept in DefaultSettingsPath so they survive restarts and upgrades.
type Settings struct {
	Location *Observer `json:"location,omitempty"`
}

// LoadSettings reads the settings file. A missing file is an empty Settings.
func LoadSettings(path string) (Settings, error) {
	var settings Settings
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	err = json.Unmarshal(raw, &settings)
	return settings, err
}

// SaveSettings writes the file through a temp file and a rename, so a crash
// mid-write leaves the old settings rather than half of the new ones.
func SaveSettings(path string, settings Settings) error {
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(directory, ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name()) // no-op once renamed
	if _, err := tempFile.Write(append(raw, '\n')); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempFile.Name(), path)
}
