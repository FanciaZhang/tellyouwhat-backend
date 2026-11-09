package voice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (s *RecordingJobStore) ownerFence(owner string) (string, error) {
	if owner == "" {
		return "", ErrInvalid
	}
	sum := sha256.Sum256([]byte(owner))
	return filepath.Join(s.root, hex.EncodeToString(sum[:])+".deleted"), nil
}
func (s *RecordingJobStore) checkOwner(owner string) error {
	path, err := s.ownerFence(owner)
	if err != nil {
		return err
	}
	_, err = os.Lstat(path)
	if err == nil {
		return os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DeleteOwner fences future and in-flight writes before erasing private content.
// Lock files remain: removing one can split mutual exclusion across processes.
func (s *RecordingJobStore) DeleteOwner(owner string) error {
	fence, err := s.ownerFence(owner)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	marker, err := os.CreateTemp(s.root, "state-")
	if err != nil {
		return err
	}
	defer os.Remove(marker.Name())
	defer marker.Close()
	if err = marker.Sync(); err != nil {
		return err
	}
	if err = marker.Close(); err != nil {
		return err
	}
	if err = os.Rename(marker.Name(), fence); err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(filepath.Base(fence), ".deleted") + "-"
	for _, entry := range entries {
		ownedContent := strings.HasPrefix(entry.Name(), prefix) && (strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".wav"))
		if !entry.IsDir() && (ownedContent || strings.HasPrefix(entry.Name(), "upload-"+prefix)) {
			if err = os.Remove(filepath.Join(s.root, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return dir.Sync()
}
