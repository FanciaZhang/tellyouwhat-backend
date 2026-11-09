package recordinghttp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

type gatedPreview struct{ entered, release chan struct{} }

func (g gatedPreview) Rewrite(_ context.Context, s voice.Snapshot, revision int) (voice.RewriteResult, error) {
	close(g.entered)
	<-g.release
	return voice.RewriteResult{Revision: voice.Revision{BaseRevision: s.Revision, TranscriptRevision: revision, Patches: []voice.Patch{{ID: s.Blocks[0].ID, Text: s.Transcript}}, Passages: []voice.PassageSource{{BlockID: s.Blocks[0].ID, ParagraphIndex: 0, SourceIDs: []string{s.RecordingContext.Analysis.Utterances[0].ID}}}, Questions: []string{}, Emotions: []voice.EmotionPlacement{}, OverallEmotion: "calm"}}, nil
}
func TestDeletedOwnerCannotReceiveOrCacheLatePreview(t *testing.T) {
	store, err := voice.NewRecordingJobStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	audio := make([]byte, 32044)
	copy(audio, "RIFF")
	binary.LittleEndian.PutUint32(audio[4:], uint32(len(audio)-8))
	copy(audio[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(audio[16:], 16)
	binary.LittleEndian.PutUint16(audio[20:], 1)
	binary.LittleEndian.PutUint16(audio[22:], 1)
	binary.LittleEndian.PutUint32(audio[24:], 16000)
	binary.LittleEndian.PutUint32(audio[28:], 32000)
	binary.LittleEndian.PutUint16(audio[32:], 2)
	binary.LittleEndian.PutUint16(audio[34:], 16)
	copy(audio[36:], "data")
	binary.LittleEndian.PutUint32(audio[40:], 32000)
	job, err := store.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(audio))
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.Advance("owner", job.ID, job.Revision, voice.RecordingSubmitting, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	analysis := voice.RecordingAnalysis{TaskID: job.ProviderTaskID, Version: voice.RecordingAnalysisVersion, Milliseconds: 1000, Text: "合成记录。", Utterances: []voice.RecordingUtterance{{ID: uuid.NewString(), Speaker: "1", Text: "合成记录。", StartMilliseconds: 0, EndMilliseconds: 900}}}
	job, err = store.Advance("owner", job.ID, job.Revision, voice.RecordingCompleted, &analysis, "")
	if err != nil {
		t.Fatal(err)
	}
	model := gatedPreview{make(chan struct{}), make(chan struct{})}
	h := New(&voice.RecordingExecutor{Store: store}, model, time.Now)
	input := PreviewRequest{RequestID: uuid.NewString(), AudioHash: job.AudioHash, ReviewRevision: 2, Snapshot: voice.Snapshot{Revision: 1, Transcript: analysis.Text, Blocks: []voice.Block{{ID: uuid.NewString(), Text: "原文"}}, RecordingContext: &voice.RecordingContext{Mode: "narrative", Analysis: analysis}}}
	body, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", Prefix+job.ID+"/preview", bytes.NewReader(body))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeAuthorized(w, r, "owner", "") }()
	select {
	case <-model.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("preview did not reach provider")
	}
	if err = store.DeleteOwner("owner"); err != nil {
		t.Fatal(err)
	}
	h.ForgetOwner("owner")
	close(model.release)
	<-done
	if w.Code != 503 || len(h.previews.entries) != 0 {
		t.Fatal("deleted preview resurrected", w.Code, len(h.previews.entries), w.Body.String())
	}
}
