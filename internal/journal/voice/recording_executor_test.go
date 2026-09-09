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
