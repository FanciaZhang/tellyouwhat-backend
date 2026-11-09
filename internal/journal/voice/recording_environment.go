package voice

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The marker also rejects accidentally mounting one environment's volume in another.
func NewEnvironmentRecordingJobStore(root, environment string, now func() time.Time) (*RecordingJobStore, error) {
	if (environment != "development" && environment != "production") || !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Base(root) != environment {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalid
	}
	err = bindRecordingEnvironment(root, environment)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	// Candidates may open storage before activation; retention is lifecycle-managed.
	return &RecordingJobStore{root: root, now: now}, nil
}
func bindRecordingEnvironment(root, environment string) error {
	store := &RecordingJobStore{root: root}
	unlock, err := store.lock()
	if err != nil {
		return err
	}
	defer unlock()
	marker := filepath.Join(root, ".environment")
	if info, err := os.Lstat(marker); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32 {
			return ErrInvalid
		}
		saved, err := os.ReadFile(marker)
		if err != nil {
			return err
		}
		if string(saved) != environment {
			return ErrInvalid
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" {
			return ErrInvalid
		}
	}
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.WriteString(environment); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
