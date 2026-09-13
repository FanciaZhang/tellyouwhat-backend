package voice

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type RecordingProvider interface {
	SubmitFile(context.Context, string, string) error
	Query(context.Context, string, int) (RecordingAnalysis, error)
}

// RecordingExecutor advances a durable job by one provider operation. The
// authorized request/worker owns scheduling; no goroutine outlives its context.
// After process restart, a submitting job is queried, never blindly submitted.
// This accounts for provider spend only; it does not touch voice-minute quota.
type RecordingExecutor struct {
	Store    *RecordingJobStore
	Provider RecordingProvider
	Budget   *costcontrol.Controller
	AppID    string
	Price    costcontrol.DurationPrice
	mu       sync.Mutex
	active   map[string]bool
}

func (e *RecordingExecutor) Process(ctx context.Context, owner, id string) (RecordingJob, error) {
	return e.advance(ctx, owner, id, 0)
}

// Retry is an explicit, revision-fenced user operation, not a polling side effect.
// Query the original task first; only a confirmed missing, never-acknowledged
// submission can return to uploaded. The provider ID and audio hash do not change.
func (e *RecordingExecutor) Retry(ctx context.Context, owner, id string, revision int) (RecordingJob, error) {
	if revision < 1 {
		return RecordingJob{}, ErrInvalid
	}
	return e.advance(ctx, owner, id, revision)
}

func (e *RecordingExecutor) advance(ctx context.Context, owner, id string, retryRevision int) (RecordingJob, error) {
	if e == nil || e.Store == nil || e.Provider == nil || e.Budget == nil || e.AppID == "" || e.Price.NanosPerHour <= 0 {
		return RecordingJob{}, ErrInvalid
	}
	key, err := e.Store.prefix(owner, id)
	if err != nil {
		return RecordingJob{}, err
	}
	e.mu.Lock()
	if e.active[key] {
		e.mu.Unlock()
		return RecordingJob{}, ErrConflict
	}
	if e.active == nil {
		e.active = make(map[string]bool)
	}
	e.active[key] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.active, key); e.mu.Unlock() }()
	if err = ctx.Err(); err != nil {
		return RecordingJob{}, err
	}
	job, err := e.Store.Get(owner, id)
	if err != nil {
		return job, err
	}
	if retryRevision > 0 {
		if job.Revision != retryRevision {
			return job, ErrConflict
		}
		if job.State != RecordingFailed || job.ErrorCode != "analysis_submission_missing" {
			return job, ErrInvalid
		}
		result, queryErr := e.Provider.Query(ctx, job.ProviderTaskID, job.Milliseconds)
		switch {
		case queryErr == nil:
			return e.Store.Advance(owner, id, job.Revision, RecordingCompleted, &result, "")
		case errors.Is(queryErr, ErrRecordingPending):
			return e.Store.Advance(owner, id, job.Revision, RecordingProcessing, nil, "")
		case errors.Is(queryErr, ErrRecordingTaskMissing):
			// Failed-job cleanup removed temporary audio; the App must restore
			// the exact same bytes before requesting a retry.
			path, err := e.Store.AudioPath(owner, id)
			if err != nil {
				return job, err
			}
			if _, err = os.Stat(path); err != nil {
				return job, err
			}
			return e.Store.Advance(owner, id, job.Revision, RecordingUploaded, nil, "")
		default:
			return job, queryErr
		}
	}
	if job.State == RecordingCompleted || job.State == RecordingFailed {
		return job, nil
	}
	if job.State == RecordingUploaded {
		path, err := e.Store.AudioPath(owner, id)
		if err != nil {
			return job, err
		}
		reserved, err := e.Price.Cost(job.Milliseconds)
		if err != nil {
			return job, err
		}
		lease, err := e.Budget.Reserve(ctx, e.AppID, "journal.voice.recording", "speech", reserved)
		if err != nil {
			// Admission failure is retryable without reuploading or changing ID.
			return job, err
		}
		job, err = e.Store.Advance(owner, id, job.Revision, RecordingSubmitting, nil, "")
		if err != nil {
			settleCost(ctx, lease, 0, true, costcontrol.Outcome{})
			return job, err
		}
		providerErr := e.Provider.SubmitFile(ctx, job.ProviderTaskID, path)
		// A duration price is a budget estimate, not a provider invoice. Retain
		// the reservation on an uncertain response, including cancellation.
		settleCost(ctx, lease, 0, false, costcontrol.Outcome{Success: providerErr == nil})
		code := ""
		if providerErr != nil {
			code = "analysis_submission_uncertain"
		}
		// Persist even if the HTTP caller disconnected during the upload. A
		// subsequent authenticated poll queries this exact provider task ID.
		next, saveErr := e.Store.Advance(owner, id, job.Revision, RecordingProcessing, nil, code)
		return next, errors.Join(providerErr, saveErr)
	}
	result, providerErr := e.Provider.Query(ctx, job.ProviderTaskID, job.Milliseconds)
	if errors.Is(providerErr, ErrRecordingPending) {
		if job.State == RecordingSubmitting || job.ErrorCode != "" {
			return e.Store.Advance(owner, id, job.Revision, RecordingProcessing, nil, "")
		}
		return job, nil
	}
	if providerErr != nil {
		if errors.Is(providerErr, ErrRecordingTaskMissing) &&
			(job.State == RecordingSubmitting || job.ErrorCode == "analysis_submission_uncertain") &&
			e.Store.now().Sub(job.UpdatedAt) >= 30*time.Second {
			// Allow propagation time after an interrupted submit. A previously
			// acknowledged task disappearing is NOT permission to resubmit.
			return e.Store.Advance(owner, id, job.Revision, RecordingFailed, nil, "analysis_submission_missing")
		}
		if errors.Is(providerErr, ErrInvalid) {
			failed, saveErr := e.Store.Advance(owner, id, job.Revision, RecordingFailed, nil, "analysis_invalid_result")
			return failed, errors.Join(providerErr, saveErr)
		}
		// A failed query is NOT proof that the submission never reached the
		// provider. No new submission or cost reservation is made here.
		return job, providerErr
	}
	next, err := e.Store.Advance(owner, id, job.Revision, RecordingCompleted, &result, "")
	if errors.Is(err, ErrInvalid) {
		failed, saveErr := e.Store.Advance(owner, id, job.Revision, RecordingFailed, nil, "analysis_invalid_result")
		return failed, errors.Join(err, saveErr)
	}
	return next, err
}

var _ RecordingProvider = RecordingASR{}
