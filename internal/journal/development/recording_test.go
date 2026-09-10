package development

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

type recordingProvider struct{ submissions, queries int }

func (p *recordingProvider) SubmitFile(_ context.Context, _ string, path string) error {
	p.submissions++
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(io.Discard, f)
	return err
}
func (p *recordingProvider) Query(_ context.Context, id string, ms int) (voice.RecordingAnalysis, error) {
	p.queries++
	return voice.RecordingAnalysis{TaskID: id, Version: voice.RecordingAnalysisVersion, Milliseconds: ms, Text: "一起去公园", Utterances: []voice.RecordingUtterance{{ID: uuid.NewString(), Speaker: "1", Text: "一起去公园", StartMilliseconds: 0, EndMilliseconds: ms, AcousticEmotion: "neutral"}}}, nil
}

func recordingWAV(ms int) []byte {
	v := make([]byte, 44+ms*32)
	copy(v, "RIFF")
	binary.LittleEndian.PutUint32(v[4:], uint32(len(v)-8))
	copy(v[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(v[16:], 16)
	binary.LittleEndian.PutUint16(v[20:], 1)
	binary.LittleEndian.PutUint16(v[22:], 1)
	binary.LittleEndian.PutUint32(v[24:], 16000)
	binary.LittleEndian.PutUint32(v[28:], 32000)
	binary.LittleEndian.PutUint16(v[32:], 2)
	binary.LittleEndian.PutUint16(v[34:], 16)
	copy(v[36:], "data")
	binary.LittleEndian.PutUint32(v[40:], uint32(len(v)-44))
	return v
}

type recordingFixture struct {
	t                               *testing.T
	h                               http.Handler
	provider                        *recordingProvider
	root, token, installation, mode string
	now                             time.Time
}

func newRecordingFixture(t *testing.T, budgetLimit int64, rewriters ...voice.Rewriter) *recordingFixture {
	t.Helper()
	f := &recordingFixture{t: t, root: t.TempDir(), token: strings.Repeat("r", 43), installation: uuid.NewString(), mode: "forced", now: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), provider: &recordingProvider{}}
	store, err := voice.NewRecordingJobStore(f.root, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: budgetLimit, MaxConcurrent: 2, LeaseDuration: time.Minute}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	var rewriter voice.Rewriter = voice.ArkRewriter{}
	if len(rewriters) > 0 {
		rewriter = rewriters[0]
	}
	f.h, err = New(Config{Token: f.token, Now: func() time.Time { return f.now }, Organizer: &model{}, Speech: voice.ASR{}, Rewriter: rewriter, Recording: &voice.RecordingExecutor{Store: store, Provider: f.provider, Budget: budget, AppID: "journal-development", Price: costcontrol.DurationPrice{NanosPerHour: 3600}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *recordingFixture) request(method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
	r.Header.Set("X-Journal-Development-Installation", f.installation)
	r.Header.Set("X-Journal-Development-Mode", f.mode)
	r.Header.Set("X-Journal-Development-Started-At", "2026-01-01T00:00:00Z")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func (f *recordingFixture) grant() {
	f.t.Helper()
	w := f.request("POST", "/v1/privacy/consents", "application/json", strings.NewReader(`{"consents":[{"scope":"managed_subscription","documentVersion":"2026-08-24","granted":true}]}`))
	if w.Code != 200 {
		f.t.Fatal(w.Code, w.Body.String())
	}
}
func jobFrom(t *testing.T, w *httptest.ResponseRecorder, status int) voice.RecordingJob {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var job voice.RecordingJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

type unreadableAudio struct{ reads int }

func (r *unreadableAudio) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestRecordingRouteAuthorizesBeforeReadingAudio(t *testing.T) {
	f := newRecordingFixture(t, 100)
	path := recordingPrefix + uuid.NewString()
	input := &unreadableAudio{}
	f.token = strings.Repeat("x", 43)
	if w := f.request("PUT", path, "audio/wav", input); w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
	f.token = strings.Repeat("r", 43)
	if w := f.request("PUT", path, "audio/wav", input); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	f.grant()
	f.mode = "monthly"
	if w := f.request("PUT", path, "audio/wav", input); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	if input.reads != 0 || f.provider.submissions != 0 {
		t.Fatal("unauthorized audio was read or sent")
	}
	entries, _ := os.ReadDir(f.root)
	if len(entries) != 0 {
		t.Fatal("unauthorized source persisted")
	}
}

func TestRecordingRouteStreamsLargeUploadAndDeduplicatesWithOwnerIsolation(t *testing.T) {
	f := newRecordingFixture(t, 201)
	f.grant()
	path := recordingPrefix + uuid.NewString()
	audio := recordingWAV(200_000) // Exceeds ordinary gateway JSON limits.
	first := jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(audio)), 202)
	again := jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(audio)), 202)
	if first.ProviderTaskID != again.ProviderTaskID || f.provider.submissions != 0 {
		t.Fatal("upload recognition/deduplication")
	}
	if w := f.request("POST", path+"/process", "", nil); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	complete := jobFrom(t, f.request("POST", path+"/process", "", nil), 200)
	if complete.Result == nil || len(complete.Result.Utterances) != 1 {
		t.Fatal(complete)
	}
	for i := 0; i < 2; i++ {
		jobFrom(t, f.request("POST", path+"/process", "", nil), 200)
	}
	jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(audio)), 200)
	if f.provider.submissions != 1 || f.provider.queries != 1 {
		t.Fatal("repeat request invoked ASR", f.provider)
	}
	audio[len(audio)-1] = 1
	if w := f.request("PUT", path, "audio/wav", bytes.NewReader(audio)); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	files, _ := filepath.Glob(filepath.Join(f.root, "*.wav"))
	if len(files) != 0 {
		t.Fatal("completed source retained")
	}
	f.mode = "monthly" // Can retrieve existing result after simulated entitlement expires.
	jobFrom(t, f.request("GET", path, "", nil), 200)
	f.mode = "forced"
	f.installation = uuid.NewString()
	f.grant()
	if w := f.request("GET", path, "", nil); w.Code != 404 {
		t.Fatal("other installation read source", w.Code, w.Body)
	}
}

func TestRecordingRouteRejectsMalformedAndExpiredResults(t *testing.T) {
	f := newRecordingFixture(t, 100)
	f.grant()
	path := recordingPrefix + uuid.NewString()
	if w := f.request("PUT", path, "audio/mpeg", strings.NewReader("bad")); w.Code != 415 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.request("PUT", path, "audio/wav", strings.NewReader("bad")); w.Code != 422 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.request("PUT", recordingPrefix+"invalid", "audio/wav", nil); w.Code != 422 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.request("POST", path+"/process", "application/json", strings.NewReader("{}")); w.Code != 422 {
		t.Fatal(w.Code, w.Body)
	}
	audio := recordingWAV(1000)
	jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(audio)), 202)
	jobFrom(t, f.request("POST", path+"/process", "", nil), 202)
	jobFrom(t, f.request("POST", path+"/process", "", nil), 200)
	f.now = f.now.Add(25 * time.Hour)
	for _, method := range []string{"GET", "PUT"} {
		var body io.Reader
		if method == "PUT" {
			body = bytes.NewReader(audio)
		}
		if w := f.request(method, path, "audio/wav", body); w.Code != 410 || strings.Contains(w.Body.String(), "公园") {
			t.Fatal("expired content exposed", w.Code, w.Body)
		}
	}
}

func TestRecordingRouteBudgetRejectsProviderCall(t *testing.T) {
	f := newRecordingFixture(t, 1)
	f.grant()
	path := recordingPrefix + uuid.NewString()
	jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(recordingWAV(2000))), 202)
	if w := f.request("POST", path+"/process", "", nil); w.Code != 429 || !strings.Contains(w.Body.String(), "recording_budget_exceeded") {
		t.Fatal(w.Code, w.Body)
	}
	if f.provider.submissions != 0 {
		t.Fatal("provider called without budget")
	}
	files, _ := filepath.Glob(filepath.Join(f.root, "*.wav"))
	if len(files) != 1 {
		t.Fatal("budget rejection discarded retry input")
	}
}

type previewRewriter struct {
	calls    int
	snapshot voice.Snapshot
}

func (p *previewRewriter) Rewrite(_ context.Context, s voice.Snapshot, revision int) (voice.RewriteResult, error) {
	p.calls++
	p.snapshot = s
	return voice.RewriteResult{Revision: voice.Revision{BaseRevision: s.Revision, TranscriptRevision: revision, Patches: []voice.Patch{{ID: s.Blocks[0].ID, Text: "妻子当时很害怕。"}}, Questions: []string{}}}, nil
}
func TestRecordingPreviewUsesOwnedSourceAndReusesSameRequest(t *testing.T) {
	model := &previewRewriter{}
	f := newRecordingFixture(t, 100, model)
	f.grant()
	id := uuid.NewString()
	path := recordingPrefix + id
	job := jobFrom(t, f.request("PUT", path, "audio/wav", bytes.NewReader(recordingWAV(1000))), 202)
	job = jobFrom(t, f.request("POST", path+"/process", "", nil), 202)
	job = jobFrom(t, f.request("POST", path+"/process", "", nil), 200)
	analysis := *job.Result
	// Foundation's UUID Codable uses uppercase, Go's uuid uses lowercase.
	analysis.TaskID = strings.ToUpper(analysis.TaskID)
	// The client's saved stream is allowed to contain a detail the file ASR lost.
	analysis.Text = "38周加一天。"
	analysis.Utterances[0].Text = analysis.Text
	analysis.Utterances[0].Speaker = "wife"
	input := recordingPreviewRequest{RequestID: uuid.NewString(), AudioHash: job.AudioHash, ReviewRevision: 7, Snapshot: voice.Snapshot{Revision: 4, Transcript: analysis.Text, Blocks: []voice.Block{{ID: uuid.NewString(), Text: "原正文"}}, RecordingContext: &voice.RecordingContext{Mode: "narrative", Speakers: []voice.RecordingSpeaker{{ID: "wife", Name: "妻子"}}, Analysis: analysis}}}
	call := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(input)
		return f.request("POST", path+"/preview", "application/json", bytes.NewReader(body))
	}
	first := call()
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body)
	}
	if second := call(); second.Code != 200 || second.Body.String() != first.Body.String() || model.calls != 1 {
		t.Fatal("duplicate preview was regenerated", second.Code, model.calls)
	}
	if model.snapshot.Transcript != "38周加一天。" {
		t.Fatal("stream detail lost")
	}
	input.Snapshot.Blocks[0].Text = "等待期间新的手改"
	if response := call(); response.Code != 409 {
		t.Fatal("reused ID accepted another snapshot", response.Code)
	}
	input.RequestID = uuid.NewString()
	input.AudioHash = strings.Repeat("b", 64)
	if response := call(); response.Code != 409 || model.calls != 1 {
		t.Fatal("wrong source reached provider", response.Code)
	}
	input.AudioHash = job.AudioHash
	f.installation = uuid.NewString()
	f.grant()
	if response := call(); response.Code != 404 {
		t.Fatal("another owner read source", response.Code)
	}
	if f.provider.submissions != 1 {
		t.Fatal("preview resubmitted audio")
	}
}
