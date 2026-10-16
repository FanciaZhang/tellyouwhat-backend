package voice

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

func TestRecordingStoreMultipleInstancesClaimOneRevision(t *testing.T) {
	root := t.TempDir()
	first, err := NewRecordingJobStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := first.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		store, err := NewRecordingJobStore(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.Advance("owner", job.ID, job.Revision, RecordingSubmitting, nil, "")
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatal("duplicate durable claim", winners.Load())
	}
}

func TestRecordingStoreCrossProcessExecutionAndCrashRecovery(t *testing.T) {
	e, job := recordingExecutorFixture(t, 1)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, binary, "-test.run=^TestRecordingStoreProcessHelper$")
	child.Env = append(os.Environ(), "JOURNAL_RECORDING_LOCK_TEST_ROOT="+e.Store.root, "JOURNAL_RECORDING_LOCK_TEST_ID="+job.ID)
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if scanner.Text() == "provider entered" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not reach provider")
		}
	case <-ctx.Done():
		t.Fatal("child startup timed out")
	}
	queries := 0
	e.Provider = recordingProviderStub{
		submit: func(context.Context, string, string) error { t.Error("ambiguous submission repeated"); return nil },
		query: func(context.Context, string, int) (RecordingAnalysis, error) {
			queries++
			return RecordingAnalysis{}, ErrRecordingPending
		},
	}
	if _, err := e.Process(context.Background(), "owner", job.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("second process entered active provider task", err)
	}
	cleaner, err := NewRecordingJobStore(e.Store.root, func() time.Time { return time.Now().Add(25 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if err = cleaner.Cleanup(); err != nil {
		t.Fatal(err)
	}
	active, err := e.Store.Get("owner", job.ID)
	if err != nil || active.State != RecordingSubmitting {
		t.Fatal("cleanup changed active task", active, err)
	}
	path, err := e.Store.AudioPath("owner", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("cleanup removed active audio", err)
	}
	if _, err = e.Store.Upload(context.Background(), "owner", job.ID, bytes.NewReader(jobWAV(1000))); !errors.Is(err, ErrConflict) {
		t.Fatal("upload overwrote active task", err)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = child.Wait(); err == nil {
		t.Fatal("child should be interrupted")
	}
	got, err := e.Process(context.Background(), "owner", job.ID)
	if err != nil || got.State != RecordingProcessing || queries != 1 || got.ProviderTaskID != job.ProviderTaskID {
		t.Fatal("crash did not release lock for query recovery", got, err, queries)
	}
	if err = cleaner.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired inactive audio retained", err)
	}
}

func TestRecordingStoreProcessHelper(t *testing.T) {
	root := os.Getenv("JOURNAL_RECORDING_LOCK_TEST_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	store, err := NewRecordingJobStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 1, MaxConcurrent: 1, LeaseDuration: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &RecordingExecutor{Store: store, Budget: budget, AppID: "journal", Price: costcontrol.DurationPrice{NanosPerHour: 3600}, Provider: recordingProviderStub{
		submit: func(context.Context, string, string) error {
			fmt.Println("provider entered")
			<-time.After(time.Hour)
			return nil
		},
	}}
	_, err = e.Process(context.Background(), "owner", os.Getenv("JOURNAL_RECORDING_LOCK_TEST_ID"))
	t.Fatal("helper unexpectedly finished", err)
}

func TestRecordingLockRejectsSymlinkAndPublicFile(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	if err := os.WriteFile(public, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(public, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{public, link} {
		if unlock, err := lockRecordingFile(path, false); err == nil {
			unlock()
			t.Fatal("unsafe lock accepted", path)
		}
	}
}
