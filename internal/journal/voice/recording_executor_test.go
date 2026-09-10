package voice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type recordingProviderStub struct {
	submit func(context.Context, string, string) error
	query  func(context.Context, string, int) (RecordingAnalysis, error)
}

func (p recordingProviderStub) SubmitFile(ctx context.Context, id, path string) error {
	return p.submit(ctx, id, path)
}
func (p recordingProviderStub) Query(ctx context.Context, id string, ms int) (RecordingAnalysis, error) {
	return p.query(ctx, id, ms)
}

func recordingExecutorFixture(t *testing.T, limit int64) (*RecordingExecutor, RecordingJob) {
	t.Helper()
	store, err := NewRecordingJobStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{
		MonthlyBudgetNanos: limit, MaxConcurrent: 2, LeaseDuration: time.Minute,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &RecordingExecutor{Store: store, Budget: budget, AppID: "journal-development", Price: costcontrol.DurationPrice{NanosPerHour: 3600}}, job
}

func TestRecordingExecutorLostSubmitResponseRecoversWithoutResubmission(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1) // Exactly one recording submission fits.
	submits, queries := 0, 0
	e.Provider = recordingProviderStub{
		submit: func(ctx context.Context, id, path string) error {
			submits++
			if id != job.ProviderTaskID {
				t.Fatal("changed task identity")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			return context.DeadlineExceeded // Provider may have accepted it.
		},
		query: func(ctx context.Context, id string, ms int) (RecordingAnalysis, error) {
			queries++
			if id != job.ProviderTaskID || ms != 1000 {
				t.Fatal("query changed identity/audio duration")
			}
			if queries == 1 {
				return RecordingAnalysis{}, ErrRecordingPending
			}
			return RecordingAnalysis{TaskID: id, Version: RecordingAnalysisVersion, Milliseconds: ms}, nil
		},
	}
	got, err := e.Process(context.Background(), "owner", job.ID)
	if !errors.Is(err, context.DeadlineExceeded) || got.State != RecordingProcessing {
		t.Fatal(got, err)
	}
	// Recreate both store and executor to discard all in-memory execution state.
	recovered, err := NewRecordingJobStore(e.Store.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = &RecordingExecutor{Store: recovered, Provider: e.Provider, Budget: e.Budget, AppID: e.AppID, Price: e.Price}
	for i := 0; i < 3; i++ {
		got, err = e.Process(context.Background(), "owner", job.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got.State != RecordingCompleted || got.Result == nil || submits != 1 || queries != 2 {
		t.Fatal(got, submits, queries)
	}
	// Completion's repeated poll neither spends the exhausted budget nor calls ASR.
	path, _ := recovered.prefix("owner", job.ID)
	if _, err = os.Stat(path + ".wav"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary source retained", err)
	}
}

func TestRecordingExecutorBudgetDenialLeavesAudioReadyForRetry(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1)
	e.Price.NanosPerHour = 7200
	e.Provider = recordingProviderStub{submit: func(context.Context, string, string) error { t.Fatal("called before admission"); return nil }}
	got, err := e.Process(context.Background(), "owner", job.ID)
	if !errors.Is(err, costcontrol.ErrBudgetExceeded) || got.State != RecordingUploaded {
		t.Fatal(got, err)
	}
	path, err := e.Store.AudioPath("owner", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("admission deleted source", err)
	}
	e.Price.NanosPerHour = 3600
	e.Provider = recordingProviderStub{submit: func(context.Context, string, string) error { return nil }}
	got, err = e.Process(context.Background(), "owner", job.ID)
	if err != nil || got.State != RecordingProcessing || got.ProviderTaskID != job.ProviderTaskID {
		t.Fatal(got, err)
	}
}

func TestRecordingExecutorFencesConcurrentCallsAndLateCompletion(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1)
	job, err := e.Store.Advance("owner", job.ID, job.Revision, RecordingSubmitting, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	e.Provider = recordingProviderStub{
		submit: func(context.Context, string, string) error {
			t.Error("restarted submitting task was resubmitted")
			return nil
		},
		query: func(_ context.Context, id string, ms int) (RecordingAnalysis, error) {
			close(started)
			<-release
			return RecordingAnalysis{TaskID: id, Version: RecordingAnalysisVersion, Milliseconds: ms}, nil
		},
	}
	go func() { _, err := e.Process(context.Background(), "owner", job.ID); done <- err }()
	<-started
	if _, err = e.Process(context.Background(), "owner", job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("concurrent query entered", err)
	}
	if _, err = e.Store.Advance("owner", job.ID, job.Revision, RecordingFailed, nil, "analysis_cancelled"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-done; !errors.Is(err, ErrConflict) {
		t.Fatal("late completion not rejected", err)
	}
	got, err := e.Store.Get("owner", job.ID)
	if err != nil || got.State != RecordingFailed || got.Result != nil {
		t.Fatal(got, err)
	}
}

func TestRecordingExecutorInvalidProviderResultFailsWithoutInventingData(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1)
	job, err := e.Store.Advance("owner", job.ID, job.Revision, RecordingSubmitting, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e.Provider = recordingProviderStub{query: func(context.Context, string, int) (RecordingAnalysis, error) {
		return RecordingAnalysis{TaskID: uuid.NewString(), Version: RecordingAnalysisVersion, Milliseconds: 1000}, nil
	}}
	got, err := e.Process(context.Background(), "owner", job.ID)
	if !errors.Is(err, ErrInvalid) || got.State != RecordingFailed || got.Result != nil || got.ErrorCode != "analysis_invalid_result" {
		t.Fatal(got, err)
	}
}

func TestRecordingMissingSubmissionRequiresExplicitFencedRetry(t *testing.T) {
	e, original := recordingExecutorFixture(t, 2)
	now := time.Now()
	e.Store.now = func() time.Time { return now }
	submits, queries := 0, 0
	e.Provider = recordingProviderStub{
		submit: func(_ context.Context, id, path string) error {
			submits++
			if id != original.ProviderTaskID {
				t.Fatal("changed provider identity")
			}
			if submits == 1 {
				return context.Canceled
			}
			return nil
		},
		query: func(_ context.Context, id string, ms int) (RecordingAnalysis, error) {
			queries++
			if submits < 2 {
				return RecordingAnalysis{}, ErrRecordingTaskMissing
			}
			return RecordingAnalysis{TaskID: id, Version: RecordingAnalysisVersion, Milliseconds: ms}, nil
		},
	}
	job, err := e.Process(context.Background(), "owner", original.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	job, err = e.Process(context.Background(), "owner", original.ID)
	if !errors.Is(err, ErrRecordingTaskMissing) || job.State != RecordingProcessing {
		t.Fatal("no propagation grace", job, err)
	}
	now = now.Add(31 * time.Second)
	job, err = e.Process(context.Background(), "owner", original.ID)
	if err != nil || job.State != RecordingFailed || job.ErrorCode != "analysis_submission_missing" {
		t.Fatal(job, err)
	}
	failedRevision := job.Revision
	// Polling a terminal failure cannot create another paid task.
	for i := 0; i < 3; i++ {
		_, _ = e.Process(context.Background(), "owner", original.ID)
	}
	if submits != 1 || queries != 2 {
		t.Fatal(submits, queries)
	}
	// Retry needs the retained local source to be uploaded with the same hash.
	if _, err = e.Retry(context.Background(), "owner", original.ID, failedRevision); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err = e.Store.Upload(context.Background(), "owner", original.ID, bytes.NewReader(jobWAV(2000))); !errors.Is(err, ErrConflict) {
		t.Fatal("changed audio accepted", err)
	}
	if _, err = e.Store.Upload(context.Background(), "owner", original.ID, bytes.NewReader(jobWAV(1000))); err != nil {
		t.Fatal(err)
	}
	job, err = e.Retry(context.Background(), "owner", original.ID, failedRevision)
	if err != nil || job.State != RecordingUploaded || job.ProviderTaskID != original.ProviderTaskID || job.AudioHash != original.AudioHash {
		t.Fatal(job, err)
	}
	// A replayed retry cannot rewind an already admitted retry.
	if _, err = e.Retry(context.Background(), "owner", original.ID, failedRevision); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	job, err = e.Process(context.Background(), "owner", original.ID)
	if err != nil || job.State != RecordingProcessing || submits != 2 {
		t.Fatal(job, err, submits)
	}
	job, err = e.Process(context.Background(), "owner", original.ID)
	if err != nil || job.State != RecordingCompleted || submits != 2 {
		t.Fatal(job, err, submits)
	}
}

func TestRecordingRetryFindsLateAcceptedTaskWithoutNewSubmission(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "pending"}[pending], func(t *testing.T) {
			e, job := recordingExecutorFixture(t, 1)
			job, _ = e.Store.Advance("owner", job.ID, job.Revision, RecordingFailed, nil, "analysis_submission_missing")
			e.Provider = recordingProviderStub{
				submit: func(context.Context, string, string) error { t.Fatal("resubmitted existing task"); return nil },
				query: func(_ context.Context, id string, ms int) (RecordingAnalysis, error) {
					if pending {
						return RecordingAnalysis{}, ErrRecordingPending
					}
					return RecordingAnalysis{TaskID: id, Version: RecordingAnalysisVersion, Milliseconds: ms}, nil
				},
			}
			got, err := e.Retry(context.Background(), "owner", job.ID, job.Revision)
			expected := RecordingCompleted
			if pending {
				expected = RecordingProcessing
			}
			if err != nil || got.State != expected {
				t.Fatal(got, err)
			}
		})
	}
}

func TestRecordingAcknowledgedTaskMissingIsNotResubmitted(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1)
	e.Provider = recordingProviderStub{
		submit: func(context.Context, string, string) error { return nil },
		query: func(context.Context, string, int) (RecordingAnalysis, error) {
			return RecordingAnalysis{}, ErrRecordingTaskMissing
		},
	}
	job, _ = e.Process(context.Background(), "owner", job.ID)
	e.Store.now = func() time.Time { return job.UpdatedAt.Add(time.Hour) }
	got, err := e.Process(context.Background(), "owner", job.ID)
	if !errors.Is(err, ErrRecordingTaskMissing) || got.State != RecordingProcessing {
		t.Fatal(got, err)
	}
	if _, err = e.Retry(context.Background(), "owner", job.ID, got.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
