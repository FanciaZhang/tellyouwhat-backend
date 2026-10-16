package voice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRecordingDeletionFencesLateProviderAndPreservesOtherOwner(t *testing.T) {
	e, job := recordingExecutorFixture(t, 100)
	other, err := e.Store.Upload(context.Background(), "other", uuid.NewString(), bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	e.Provider = recordingProviderStub{submit: func(context.Context, string, string) error { close(entered); <-release; return nil }}
	go func() { _, err := e.Process(context.Background(), "owner", job.ID); done <- err }()
	<-entered
	if err := e.Store.DeleteOwner("owner"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late outcome was saved", err)
	}
	for range 2 {
		if err := e.Store.DeleteOwner("owner"); err != nil {
			t.Fatal("deletion not repeatable", err)
		}
	}
	if _, err := e.Store.Get("other", other.ID); err != nil {
		t.Fatal("other owner erased", err)
	}
	reopened, err := NewRecordingJobStore(e.Store.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(jobWAV(1000))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted owner recreated content", err)
	}
	if _, err = reopened.Get("owner", job.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted task accessible", err)
	}
}

type pausedRecordingReader struct {
	reader           io.Reader
	entered, release chan struct{}
	waited           bool
}

func (r *pausedRecordingReader) Read(b []byte) (int, error) {
	if !r.waited {
		r.waited = true
		close(r.entered)
		<-r.release
	}
	return r.reader.Read(b)
}
func TestRecordingDeletionFencesAlreadyStartedUpload(t *testing.T) {
	store, err := NewRecordingJobStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	source := &pausedRecordingReader{reader: bytes.NewReader(jobWAV(1000)), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := store.Upload(context.Background(), "owner", uuid.NewString(), source); done <- err }()
	<-source.entered
	if err = store.DeleteOwner("owner"); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(store.root, "upload-*")); len(files) != 0 {
		t.Fatal("active upload remains reachable after deletion", files)
	}
	close(source.release)
	if err = <-done; !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late upload recreated content", err)
	}
	for _, pattern := range []string{"*.wav", "*.json", "upload-*", "state-*"} {
		files, _ := filepath.Glob(filepath.Join(store.root, pattern))
		if len(files) > 0 {
			t.Fatal("deleted content remains", files)
		}
	}
}
func TestSharedRecordingEnvironmentMarkerRejectsWrongMountBeforeCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "development")
	store, err := NewEnvironmentRecordingJobStore(root, "development", nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".environment"), []byte("production"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewEnvironmentRecordingJobStore(root, "development", func() time.Time { return time.Now().Add(48 * time.Hour) }); err == nil {
		t.Fatal("foreign volume accepted")
	}
	if got, err := store.Get("owner", job.ID); err != nil || got.State != RecordingUploaded {
		t.Fatal("foreign volume cleaned before rejection", got, err)
	}
	if _, err = NewEnvironmentRecordingJobStore(root, "production", nil); err == nil {
		t.Fatal("environment path mismatch accepted")
	}
}
func TestSharedRecordingStoreDoesNotAdoptPrivateOrPublicDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "development")
	store, err := NewRecordingJobStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Upload(context.Background(), "private-owner", uuid.NewString(), bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewEnvironmentRecordingJobStore(root, "development", nil); err == nil {
		t.Fatal("private tasks silently adopted")
	}
	fresh := filepath.Join(t.TempDir(), "production")
	if err = os.Mkdir(fresh, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = NewEnvironmentRecordingJobStore(fresh, "production", nil); err == nil {
		t.Fatal("public processing directory accepted")
	}
}
