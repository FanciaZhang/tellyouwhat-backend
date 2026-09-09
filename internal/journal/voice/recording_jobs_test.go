package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func jobWAV(milliseconds int) []byte {
	wav := make([]byte, 44+milliseconds*32)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(wav)-44))
	return wav
}
func TestRecordingJobUploadDeduplicatesPersistsAndSeparatesOwners(t *testing.T) {
	root := t.TempDir()
	store, err := NewRecordingJobStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	wav := jobWAV(1000)
	first, err := store.Upload(context.Background(), "owner-a", id, bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Upload(context.Background(), "owner-a", id, bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderTaskID != duplicate.ProviderTaskID || first.Revision != duplicate.Revision {
		t.Fatal("duplicate paid task")
	}
	recovered, err := NewRecordingJobStore(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recovered.Get("owner-a", id)
	if err != nil || got.ProviderTaskID != first.ProviderTaskID {
		t.Fatal(got, err)
	}
	if _, err = recovered.Get("owner-b", id); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cross-owner lookup", err)
	}
	wav[len(wav)-1] = 1
	if _, err = recovered.Upload(context.Background(), "owner-a", id, bytes.NewReader(wav)); !errors.Is(err, ErrConflict) {
		t.Fatal("mismatched duplicate", err)
	}
	if _, err = recovered.Get("owner-a", "../../secret"); !errors.Is(err, ErrInvalid) {
		t.Fatal("path traversal", err)
	}
}
func TestRecordingJobCompletionFencesLateResultsAndRemovesTemporaryAudio(t *testing.T) {
	store, _ := NewRecordingJobStore(t.TempDir(), func() time.Time { return time.Unix(1000, 0) })
	id := uuid.NewString()
	job, err := store.Upload(context.Background(), "owner", id, bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	audioPath, err := store.AudioPath("owner", id)
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.Advance("owner", id, job.Revision, RecordingSubmitting, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	result := RecordingAnalysis{TaskID: job.ProviderTaskID, Version: RecordingAnalysisVersion, Milliseconds: 1000, Text: "我害怕", Utterances: []RecordingUtterance{{ID: uuid.NewString(), Speaker: "1", Text: "我害怕", StartMilliseconds: 0, EndMilliseconds: 900, AcousticEmotion: "happy"}}}
	if _, err = store.Advance("owner", id, 1, RecordingCompleted, &result, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("late completion accepted", err)
	}
	job, err = store.Advance("owner", id, job.Revision, RecordingCompleted, &result, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(audioPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("audio retained after completion")
	}
	got, err := store.Get("owner", id)
	if err != nil || got.Result.Utterances[0].AcousticEmotion != "happy" {
		t.Fatal(got, err)
	}
	if _, err = store.AudioPath("owner", id); !errors.Is(err, ErrInvalid) {
		t.Fatal("completed audio available")
	}
	// Replaying an upload cannot restart recognition or recreate deleted audio.
	again, err := store.Upload(context.Background(), "owner", id, bytes.NewReader(jobWAV(1000)))
	if err != nil || again.ProviderTaskID != job.ProviderTaskID {
		t.Fatal(again, err)
	}
	if _, err = os.Stat(audioPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("duplicate recreated audio")
	}
	files, _ := filepath.Glob(filepath.Join(store.root, "upload-*"))
	if len(files) != 0 {
		t.Fatal("upload temporary files retained")
	}
}
func TestRecordingJobFailureRetainsIdentityAndRetryRestagesSameAudio(t *testing.T) {
	store, _ := NewRecordingJobStore(t.TempDir(), nil)
	id := uuid.NewString()
	job, err := store.Upload(context.Background(), "owner", id, bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := store.AudioPath("owner", id)
	job, err = store.Advance("owner", id, job.Revision, RecordingFailed, nil, "provider_unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(audio); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed task audio retained")
	}
	retry, err := store.Upload(context.Background(), "owner", id, bytes.NewReader(jobWAV(1000)))
	if err != nil || retry.ProviderTaskID != job.ProviderTaskID {
		t.Fatal(retry, err)
	}
	if _, err = os.Stat(audio); err != nil {
		t.Fatal("retry audio missing", err)
	}
	if _, err = store.Advance("owner", id, retry.Revision, RecordingSubmitting, nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("retry skipped query", err)
	}
	if _, err = store.Advance("owner", id, retry.Revision, RecordingProcessing, nil, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingJobRestartFinishesCleanupAndExpiresDeliveryContent(t *testing.T) {
	now := time.Unix(1000, 0)
	root := t.TempDir()
	store, _ := NewRecordingJobStore(root, func() time.Time { return now })
	id := uuid.NewString()
	job, err := store.Upload(context.Background(), "owner", id, bytes.NewReader(jobWAV(1000)))
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := store.AudioPath("owner", id)
	job, err = store.Advance("owner", id, job.Revision, RecordingSubmitting, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	result := RecordingAnalysis{TaskID: job.ProviderTaskID, Version: RecordingAnalysisVersion, Milliseconds: 1000}
	job, err = store.Advance("owner", id, job.Revision, RecordingCompleted, &result, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(audio, jobWAV(1000), 0600); err != nil {
		t.Fatal(err)
	} // crash after publishing completion, before cleanup
	recovered, err := NewRecordingJobStore(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(audio); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restart retained completed audio")
	}
	now = now.Add(25 * time.Hour)
	if err = recovered.Cleanup(); err != nil {
		t.Fatal(err)
	}
	got, err := recovered.Get("owner", id)
	if err != nil || got.Result != nil || got.ProviderTaskID != job.ProviderTaskID || got.State != RecordingCompleted {
		t.Fatal(got, err)
	}
}
