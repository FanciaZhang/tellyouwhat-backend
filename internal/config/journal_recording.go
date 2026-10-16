package config

import (
	"errors"
	"os"
	"path/filepath"
)

type JournalRecordingConfig struct {
	Enabled bool
	Root    string
}

// Keep loading the directory when generation is off so deletion and retention still run.
func LoadJournalRecording(environment string) (*JournalRecordingConfig, error) {
	flag, root := os.Getenv("JOURNAL_RECORDING_ENABLED"), os.Getenv("JOURNAL_RECORDING_ROOT")
	if flag != "" && flag != "true" && flag != "false" {
		return nil, errors.New("JOURNAL_RECORDING_ENABLED must be true or false")
	}
	if root == "" && flag != "true" {
		return nil, nil
	}
	if (environment != "development" && environment != "production") || !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Base(root) != environment {
		return nil, errors.New("JOURNAL_RECORDING_ROOT must be an absolute directory ending in the exact APP_ENV")
	}
	return &JournalRecordingConfig{Enabled: flag == "true", Root: root}, nil
}
