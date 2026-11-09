package voice

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Keep lock files: unlinking one lets another process lock a different inode.
func lockRecordingFile(path string, nonblocking bool) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, ErrInvalid
	}
	mode := syscall.LOCK_EX
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(fd, mode); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = file.Close() }, nil
}

func (s *RecordingJobStore) lock() (func(), error) {
	return lockRecordingFile(filepath.Join(s.root, ".lock"), false)
}

func (s *RecordingJobStore) lockExecution(owner, id string) (func(), error) {
	path, err := s.prefix(owner, id)
	if err != nil {
		return nil, err
	}
	// Unknown requests must not populate the store with permanent lock files.
	if _, err := s.Get(owner, id); err != nil {
		return nil, err
	}
	return lockRecordingFile(path+".execution.lock", true)
}
