package voice

import (
	"context"
	"errors"
	"sync"

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
