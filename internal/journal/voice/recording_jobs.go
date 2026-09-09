package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type RecordingJobState string

const (
	RecordingUploaded   RecordingJobState = "uploaded"
	RecordingSubmitting RecordingJobState = "submitting"
	RecordingProcessing RecordingJobState = "processing"
	RecordingCompleted  RecordingJobState = "completed"
	RecordingFailed     RecordingJobState = "failed"
)

// ProviderTaskID is persisted before any paid submission. Revision fences stop
// late workers from overwriting a newer retry/cancellation decision.
type RecordingJob struct {
	ID             string             `json:"id"`
	ProviderTaskID string             `json:"providerTaskID"`
	AudioHash      string             `json:"audioHash"`
	Milliseconds   int                `json:"milliseconds"`
	State          RecordingJobState  `json:"state"`
	Revision       int                `json:"revision"`
	UpdatedAt      time.Time          `json:"updatedAt"`
	Result         *RecordingAnalysis `json:"result,omitempty"`
	ErrorCode      string             `json:"errorCode,omitempty"`
}

// This store is private temporary processing storage, not journal sync/storage.
// The authenticated caller supplies owner; no client-supplied filesystem path
// is accepted. One process owns a store directory in the Journal development service.
type RecordingJobStore struct {
	root string
	now  func() time.Time
	mu   sync.Mutex
}

func NewRecordingJobStore(root string, now func() time.Time) (*RecordingJobStore, error) {
	if root == "" {
		return nil, ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	store := &RecordingJobStore{root: root, now: now}
	if err := store.Cleanup(); err != nil {
		return nil, err
	}
	return store, nil
}
func (s *RecordingJobStore) prefix(owner, id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if owner == "" || err != nil || len(id) != 36 {
		return "", ErrInvalid
	}
	digest := sha256.Sum256([]byte(owner))
	return filepath.Join(s.root, hex.EncodeToString(digest[:])+"-"+parsed.String()), nil
}
func readRecordingJob(path string) (RecordingJob, error) {
	file, err := os.Open(path + ".json")
	if err != nil {
		return RecordingJob{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxRecordingResponseBytes+1))
	if err != nil || len(raw) > maxRecordingResponseBytes {
		return RecordingJob{}, ErrInvalid
	}
	var job RecordingJob
	if json.Unmarshal(raw, &job) != nil {
		return job, ErrInvalid
	}
	if _, err := uuid.Parse(job.ID); err != nil {
		return job, ErrInvalid
	}
	if _, err := uuid.Parse(job.ProviderTaskID); err != nil {
		return job, ErrInvalid
	}
	digest, err := hex.DecodeString(job.AudioHash)
	if err != nil || len(digest) != 32 || job.Revision < 1 || job.Milliseconds < 1 || job.Milliseconds > SessionMilliseconds || job.UpdatedAt.IsZero() || len(job.ErrorCode) > 128 {
		return job, ErrInvalid
	}
	switch job.State {
	case RecordingUploaded, RecordingSubmitting, RecordingProcessing, RecordingCompleted, RecordingFailed:
	default:
		return job, ErrInvalid
	}
	return job, nil
}
func (s *RecordingJobStore) Get(owner, id string) (RecordingJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.prefix(owner, id)
	if err != nil {
		return RecordingJob{}, err
	}
	return readRecordingJob(path)
}

// Upload hashes actual bytes and atomically publishes only validated canonical
// audio. Repeated IDs with identical bytes reuse the original provider task.
func (s *RecordingJobStore) Upload(ctx context.Context, owner, id string, input io.Reader) (RecordingJob, error) {
	path, err := s.prefix(owner, id)
	if err != nil {
		return RecordingJob{}, err
	}
	temp, err := os.CreateTemp(s.root, "upload-*")
	if err != nil {
		return RecordingJob{}, err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(input, SessionMilliseconds*64+45))
	if err != nil {
		return RecordingJob{}, err
	}
	if err = ctx.Err(); err != nil {
		return RecordingJob{}, err
	}
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		return RecordingJob{}, err
	}
	header := make([]byte, 44)
	if _, err = io.ReadFull(temp, header); err != nil {
		return RecordingJob{}, ErrInvalid
	}
	milliseconds := recordingWAVDuration(header, n)
	if milliseconds == 0 {
		return RecordingJob{}, ErrInvalid
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := readRecordingJob(path)
	if err == nil {
		if previous.AudioHash != sum {
			return RecordingJob{}, ErrConflict
		}
		if previous.State == RecordingFailed {
			if err = temp.Sync(); err != nil {
				return RecordingJob{}, err
			}
			if err = temp.Close(); err != nil {
				return RecordingJob{}, err
			}
			if err = os.Rename(temp.Name(), path+".wav"); err != nil {
				return RecordingJob{}, err
			}
		}
		return previous, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return RecordingJob{}, err
	}
	if err = temp.Sync(); err != nil {
		return RecordingJob{}, err
	}
	if err = temp.Close(); err != nil {
		return RecordingJob{}, err
	}
	if err = os.Rename(temp.Name(), path+".wav"); err != nil {
		return RecordingJob{}, err
	}
	job := RecordingJob{ID: id, ProviderTaskID: uuid.NewString(), AudioHash: sum, Milliseconds: milliseconds, State: RecordingUploaded, Revision: 1, UpdatedAt: s.now()}
	if err = s.write(path, job); err != nil {
		return RecordingJob{}, err
	}
	return job, nil
}
func (s *RecordingJobStore) write(path string, job RecordingJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if len(data) > maxRecordingResponseBytes {
		return ErrInvalid
	}
	file, err := os.CreateTemp(s.root, "state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path+".json"); err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *RecordingJobStore) Advance(owner, id string, revision int, state RecordingJobState, result *RecordingAnalysis, errorCode string) (RecordingJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.prefix(owner, id)
	if err != nil {
		return RecordingJob{}, err
	}
	job, err := readRecordingJob(path)
	if err != nil {
		return job, err
	}
	if job.Revision != revision {
		return job, ErrConflict
	}
	allowed := map[RecordingJobState]map[RecordingJobState]bool{
		RecordingUploaded:   {RecordingSubmitting: true, RecordingFailed: true},
		RecordingSubmitting: {RecordingProcessing: true, RecordingCompleted: true, RecordingFailed: true},
		RecordingProcessing: {RecordingProcessing: true, RecordingCompleted: true, RecordingFailed: true},
		// Retry always queries the persisted provider task before deciding to submit.
		RecordingFailed: {RecordingProcessing: true},
	}
	if !allowed[job.State][state] || len(errorCode) > 128 {
		return job, ErrInvalid
	}
	if state == RecordingCompleted {
		if result == nil || result.TaskID != job.ProviderTaskID || absDuration(result.Milliseconds-job.Milliseconds) > 100 {
			return job, ErrInvalid
		}
		if err := (RecordingContext{Mode: "narrative", Analysis: *result}).Validate(result.Text); err != nil {
			return job, err
		}
	} else if result != nil {
		return job, ErrInvalid
	}
	job.State = state
	job.Result = result
	job.ErrorCode = errorCode
	job.Revision++
	job.UpdatedAt = s.now()
	if err = s.write(path, job); err != nil {
		return job, err
	}
	// Publish the result durably BEFORE deleting temporary audio. Cleanup can
	// repeat safely after a crash between these two operations.
	if state == RecordingCompleted || state == RecordingFailed {
		if err = os.Remove(path + ".wav"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return job, err
		}
	}
	return job, nil
}
func absDuration(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (s *RecordingJobStore) AudioPath(owner, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.prefix(owner, id)
	if err != nil {
		return "", err
	}
	job, err := readRecordingJob(path)
	if err != nil {
		return "", err
	}
	if job.State == RecordingCompleted {
		return "", ErrInvalid
	}
	return path + ".wav", nil
}

// Cleanup is repeatable after process termination. Result content is a temporary
// delivery cache (24 hours), while its small identity/hash tombstone prevents
// accidental re-submission. The App owns the durable document/result.
func (s *RecordingJobStore) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(s.root, name)
		if strings.HasPrefix(name, "upload-") || strings.HasPrefix(name, "state-") {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if s.now().Sub(info.ModTime()) > time.Hour {
				if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			continue
		}
		if strings.HasSuffix(name, ".wav") {
			prefix := strings.TrimSuffix(path, ".wav")
			if _, err := os.Stat(prefix + ".json"); errors.Is(err, os.ErrNotExist) {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if s.now().Sub(info.ModTime()) > time.Hour {
					if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		prefix := strings.TrimSuffix(path, ".json")
		job, err := readRecordingJob(prefix)
		if err != nil {
			return err
		}
		if s.now().Sub(job.UpdatedAt) > 24*time.Hour && (job.Result != nil || job.State != RecordingCompleted && job.State != RecordingFailed) {
			if job.State != RecordingCompleted {
				job.State = RecordingFailed
			}
			job.Result = nil
			job.ErrorCode = "analysis_result_expired"
			job.Revision++
			job.UpdatedAt = s.now()
			if err = s.write(prefix, job); err != nil {
				return err
			}
		}
		if job.State == RecordingCompleted || job.State == RecordingFailed {
			if err = os.Remove(prefix + ".wav"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
