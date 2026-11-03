package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/recordinghttp"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
)

type recordingProofRecorder struct {
	proof RequestProof
	app   string
}

func (a *recordingProofRecorder) Authenticate(ctx context.Context, p RequestProof) (Principal, error) {
	a.proof = p
	return (fakeAuthenticator{appID: a.app}).Authenticate(ctx, p)
}

type recordingASRFixture struct{ submits, queries int }

func (p *recordingASRFixture) SubmitFile(context.Context, string, string) error {
	p.submits++
	return nil
}
func (p *recordingASRFixture) Query(_ context.Context, id string, ms int) (voice.RecordingAnalysis, error) {
	p.queries++
	return voice.RecordingAnalysis{TaskID: id, Version: voice.RecordingAnalysisVersion, Milliseconds: ms}, nil
}
func recordingWAVFixture(ms int) []byte {
	b := make([]byte, 44+ms*32)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(b)-44))
	return b
}
func recordingGatewayFixture(t *testing.T) (*Server, *voice.RecordingJobStore, *recordingASRFixture, *recordingProofRecorder) {
	t.Helper()
	store, err := voice.NewRecordingJobStore(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 1_000_000, MaxConcurrent: 2, LeaseDuration: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingASRFixture{}
	executor := &voice.RecordingExecutor{Store: store, Provider: provider, Budget: budget, AppID: "journal", Price: costcontrol.DurationPrice{NanosPerHour: 3600}}
	auth := &recordingProofRecorder{app: "journal"}
	s := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: auth, Entitlements: fakeEntitlements{allowed: true}, Consent: fakeConsentGate{granted: true}, JournalRecordings: recordinghttp.New(executor, recordingRewriterFixture{}, time.Now)})
	return s, store, provider, auth
}
func recordingUploadRequest(id string, b []byte) *http.Request {
	r := authorizedRequest(http.MethodPut, recordinghttp.Prefix+id, "")
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Type", "audio/wav")
	digest := sha256.Sum256(b)
	r.Header.Set(recordinghttp.DigestHeader, hex.EncodeToString(digest[:]))
	return r
}

type unreadRecordingBody struct{ reads int }

func (b *unreadRecordingBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadRecordingBody) Close() error             { return nil }

func TestSharedRecordingRejectsBeforeReadingAudio(t *testing.T) {
	for _, mode := range []string{"no assertion", "wrong app", "no entitlement", "no consent", "missing digest", "invalid digest", "not enabled"} {
		t.Run(mode, func(t *testing.T) {
			s, _, p, a := recordingGatewayFixture(t)
			r := recordingUploadRequest(uuid.NewString(), nil)
			body := &unreadRecordingBody{}
			r.Body = body
			r.ContentLength = -1
			switch mode {
			case "no assertion":
				r.Header.Del("X-Tellyouwhat-Assertion")
			case "wrong app":
				a.app = "health"
			case "no entitlement":
				s.entitlements = fakeEntitlements{}
			case "no consent":
				s.consent = fakeConsentGate{}
			case "missing digest":
				r.Header.Del(recordinghttp.DigestHeader)
			case "invalid digest":
				r.Header.Set(recordinghttp.DigestHeader, "bad")
			case "not enabled":
				s.journalRecordings = nil
			}
			w := httptest.NewRecorder()
			s.Router().ServeHTTP(w, r)
			if w.Code < 400 || body.reads != 0 || p.submits != 0 {
				t.Fatal(mode, w.Code, body.reads, w.Body.String())
			}
		})
	}
}
func TestSharedRecordingStreamsVerifiesAndRecoversOriginalTask(t *testing.T) {
	s, store, p, a := recordingGatewayFixture(t)
	id := uuid.NewString()
	audio := recordingWAVFixture(200_000)
	r := recordingUploadRequest(id, audio)
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 202 || a.proof.BodySHA256 != r.Header.Get(recordinghttp.DigestHeader) || p.submits != 0 {
		t.Fatal(w.Code, w.Body.String(), a.proof)
	}
	var first voice.RecordingJob
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("valid-key", id); err != nil {
		t.Fatal("authenticated owner was not used", err)
	}
	for i := 0; i < 3; i++ {
		w = httptest.NewRecorder()
		s.Router().ServeHTTP(w, authorizedRequest(http.MethodPost, recordinghttp.Prefix+id+"/process", ""))
		if w.Code != 202 && w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if p.submits != 1 || p.queries != 1 {
		t.Fatal("duplicated paid request", p)
	}
	s.entitlements = fakeEntitlements{}
	w = httptest.NewRecorder()
	s.Router().ServeHTTP(w, authorizedRequest(http.MethodGet, recordinghttp.Prefix+id, ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), first.ProviderTaskID) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("expired subscription lost owned result", w.Code, w.Body.String())
	}
	other := authorizedRequest(http.MethodGet, recordinghttp.Prefix+id, "")
	other.Header.Set("X-Tellyouwhat-Key-ID", "other-owner")
	w = httptest.NewRecorder()
	s.Router().ServeHTTP(w, other)
	if w.Code != 404 {
		t.Fatal("cross-owner result", w.Code)
	}
}
func TestSharedRecordingRejectsTamperedSignedBytes(t *testing.T) {
	s, store, p, _ := recordingGatewayFixture(t)
	id := uuid.NewString()
	audio := recordingWAVFixture(1000)
	r := recordingUploadRequest(id, audio)
	audio[len(audio)-1] = 1
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 422 || p.submits != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := store.Get("valid-key", id); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tampered content persisted", err)
	}
}

type recordingRewriterFixture struct{}

func (recordingRewriterFixture) Rewrite(_ context.Context, s voice.Snapshot, revision int) (voice.RewriteResult, error) {
	return voice.RewriteResult{Revision: voice.Revision{BaseRevision: s.Revision, TranscriptRevision: revision, Patches: []voice.Patch{{ID: s.Blocks[0].ID, Text: "合成记录。"}}, Passages: []voice.PassageSource{{BlockID: s.Blocks[0].ID, ParagraphIndex: 0, SourceIDs: []string{s.RecordingContext.Analysis.Utterances[0].ID}}}, Questions: []string{}, Emotions: []voice.EmotionPlacement{}, OverallEmotion: "calm"}}, nil
}
func TestSharedRecordingJSONAdaptersPreserveAuthenticatedPreviewAndRetry(t *testing.T) {
	s, store, _, a := recordingGatewayFixture(t)
	id := uuid.NewString()
	router := s.Router()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, recordingUploadRequest(id, recordingWAVFixture(1000)))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	for range 2 {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, authorizedRequest(http.MethodPost, recordinghttp.Prefix+id+"/process", ""))
	}
	job, err := store.Get("valid-key", id)
	if err != nil || job.Result == nil {
		t.Fatal(job, err)
	}
	analysis := *job.Result
	analysis.Text = "合成记录。"
	analysis.Utterances = []voice.RecordingUtterance{{ID: uuid.NewString(), Speaker: "1", Text: analysis.Text, StartMilliseconds: 0, EndMilliseconds: 900}}
	input := recordinghttp.PreviewRequest{RequestID: uuid.NewString(), AudioHash: job.AudioHash, ReviewRevision: 2, Snapshot: voice.Snapshot{Revision: 1, Transcript: analysis.Text, Blocks: []voice.Block{{ID: uuid.NewString(), Text: "原文"}}, RecordingContext: &voice.RecordingContext{Mode: "narrative", Analysis: analysis}}}
	body, _ := json.Marshal(input)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authorizedRequest(http.MethodPost, recordinghttp.Prefix+id+"/preview", string(body)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "合成记录") {
		t.Fatal("preview reader lost after generated binding", w.Code, w.Body.String())
	}
	digest := sha256.Sum256(body)
	if a.proof.BodySHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("preview auth did not bind bytes")
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, authorizedRequest(http.MethodPost, recordinghttp.Prefix+id+"/retry", `{"revision":1}`))
	if w.Code != 409 {
		t.Fatal("retry revision lost after generated binding", w.Code, w.Body.String())
	}
}
