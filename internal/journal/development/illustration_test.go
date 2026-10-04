package development

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type imageFixture struct {
	calls   atomic.Int32
	before  func()
	failure error
}

func (g *imageFixture) Model() string { return "fixture-image" }
func (g *imageFixture) Generate(context.Context, illustration.Input) (illustration.Result, error) {
	g.calls.Add(1)
	if g.before != nil {
		g.before()
	}
	if g.failure != nil {
		return illustration.Result{}, g.failure
	}
	var data bytes.Buffer
	_ = png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 3)))
	return illustration.Result{Image: data.Bytes(), MIME: "image/png", Width: 4, Height: 3}, nil
}

func TestIllustrationProviderFailureReportsUnavailableWithoutRedispatch(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			now := time.Now()
			g := &imageFixture{failure: illustration.Rejected{Status: status}}
			runtime, err := NewIllustrationRuntime(t.TempDir(), strings.Repeat("i", 43), g, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			runtime.authorize = func(context.Context, string) bool { return true }
			id := uuid.NewString()
			_, err = runtime.create("owner", illustration.Confirmation{RequestID: id, VersionID: id, Prompt: "Synthetic tree", ConsentRevision: "2026-09-27"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime.process(context.Background(), runtime.path("owner", id))
			runtime.process(context.Background(), runtime.path("owner", id))
			if g.calls.Load() != 1 {
				t.Fatal("rejection redispatched")
			}
			r := httptest.NewRequest("GET", illustrationPrefix+"/"+id, nil)
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, attestation.Principal{AppID: "journal", KeyID: "owner"}))
			w := httptest.NewRecorder()
			runtime.ServeHTTP(w, r)
			var response imageTaskResponse
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
				t.Fatal(w.Code, w.Body)
			}
			want := "unavailable"
			if status == 429 {
				want = "quota"
			}
			if response.State != illustration.TaskRejected || response.Problem != want {
				t.Fatal(response)
			}
			job, err := runtime.load("owner", id)
			if err != nil || job.Task.Charge != illustration.ChargeUncertain || job.Input != nil {
				t.Fatal("failure lost privacy or billing fence", err)
			}
		})
	}
}

func TestIllustrationHTTPConsentDurabilityIsolationAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	root := t.TempDir()
	token := strings.Repeat("i", 43)
	g := &imageFixture{}
	runtime, err := NewIllustrationRuntime(root, token, g, clock)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Token: token, Now: clock, Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}, Illustrations: runtime})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	requestID := uuid.NewString()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Tellyouwhat-Request-ID", requestID)
		r.Header.Set("X-Journal-Development-Installation", owner)
		r.Header.Set("X-Journal-Development-Mode", "forced")
		r.Header.Set("X-Journal-Development-Started-At", "2026-10-01T00:00:00Z")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	body := `{"requestID":"` + requestID + `","versionID":"` + requestID + `","prompt":"Synthetic dog by the river","consentRevision":"2026-09-27"}`
	if w := request("POST", illustrationPrefix, body); w.Code != 403 {
		t.Fatalf("missing consent: %d %s", w.Code, w.Body)
	}
	grant := `{"consents":[{"scope":"journal_illustration","documentVersion":"2026-09-27","granted":true}]}`
	if w := request("POST", "/v1/privacy/consents", grant); w.Code != 200 {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}
	var accepted imageTaskResponse
	w := request("POST", illustrationPrefix, body)
	if w.Code != 200 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &accepted)
	if accepted.State != illustration.TaskQueued || accepted.ID != requestID {
		t.Fatal(accepted)
	}
	if w = request("POST", illustrationPrefix, body); w.Code != 200 {
		t.Fatalf("replay %d", w.Code)
	}
	if w = request("POST", illustrationPrefix, strings.Replace(body, "river", "forest", 1)); w.Code != 409 {
		t.Fatalf("changed material %d", w.Code)
	}
	allEntries, _ := os.ReadDir(root)
	var entries []os.DirEntry
	for _, entry := range allEntries {
		if strings.HasSuffix(entry.Name(), ".job") {
			entries = append(entries, entry)
		}
	}
	if len(entries) != 1 {
		t.Fatal("duplicate created extra jobs")
	}
	path := filepath.Join(root, entries[0].Name())
	ciphertext, _ := os.ReadFile(path)
	if bytes.Contains(ciphertext, []byte("Synthetic dog")) {
		t.Fatal("plaintext stored")
	}
	runtime.process(context.Background(), path)
	runtime.process(context.Background(), path)
	if g.calls.Load() != 1 {
		t.Fatal("generation repeated")
	}
	w = request("GET", illustrationPrefix+"/"+requestID+"/result", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mime":"image/png"`) {
		t.Fatalf("result %d %s", w.Code, w.Body)
	}
	restarted, err := NewIllustrationRuntime(root, token, g, clock)
	if err != nil {
		t.Fatal(err)
	}
	restarted.authorize = runtime.authorize
	runtime = restarted
	handler, err = New(Config{Token: token, Now: clock, Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}, Illustrations: restarted})
	if err != nil {
		t.Fatal(err)
	}
	if w = request("GET", illustrationPrefix+"/"+requestID+"/result", ""); w.Code != 200 {
		t.Fatalf("approved access lost after restart %d %s", w.Code, w.Body)
	}
	restarted.process(context.Background(), path)
	if g.calls.Load() != 1 {
		t.Fatal("restart redispatched")
	}
	oldOwner := owner
	owner = uuid.NewString()
	w = request("GET", illustrationPrefix+"/"+requestID, "")
	if w.Code != 403 {
		t.Fatalf("new identity must require own consent %d", w.Code)
	}
	if w = request("POST", "/v1/privacy/consents", grant); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request("GET", illustrationPrefix+"/"+requestID, ""); w.Code != 404 {
		t.Fatalf("cross-owner %d", w.Code)
	}
	owner = oldOwner
	now = now.Add(25 * time.Hour)
	restarted.process(context.Background(), path)
	job, err := restarted.loadPath(path)
	if err != nil || job.Result != nil || job.Input != nil || job.Task.State != illustration.TaskCancelled {
		t.Fatalf("expiry %v %v", job, err)
	}
	if w = request("POST", illustrationPrefix, body); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if g.calls.Load() != 1 {
		t.Fatal("expired identity reused")
	}
}

func TestIllustrationConsentWithdrawalSurvivesRestart(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	token := strings.Repeat("i", 43)
	runtime, err := NewIllustrationRuntime(root, token, &imageFixture{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	repository, err := newImageConsentRepository(runtime)
	if err != nil {
		t.Fatal(err)
	}
	record := privacy.Record{KeyID: "owner", DeviceID: "device", Scope: privacy.JournalIllustrationScope, DocumentVersion: privacy.JournalIllustrationDocumentVersion, Granted: true, RecordedAt: now}
	if err = repository.RecordConsents(context.Background(), []privacy.Record{record}); err != nil {
		t.Fatal(err)
	}
	record.Granted = false
	if err = repository.RecordConsents(context.Background(), []privacy.Record{record}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewIllustrationRuntime(root, token+"rotated", &imageFixture{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	reader, err := newImageConsentRepository(restarted)
	if err != nil {
		t.Fatal(err)
	}
	granted, err := reader.(privacy.ConsentReader).HasGrantedConsents(context.Background(), "owner", []privacy.Consent{{Scope: privacy.JournalIllustrationScope, DocumentVersion: privacy.JournalIllustrationDocumentVersion, Granted: true}})
	if err != nil || granted {
		t.Fatal("withdrawn consent restored")
	}
}

func TestIllustrationRunningRestartAndCancellationFence(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	token := strings.Repeat("i", 43)
	g := &imageFixture{}
	runtime, err := NewIllustrationRuntime(root, token, g, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runtime.authorize = func(context.Context, string) bool { return true }
	owner := uuid.NewString()
	id := uuid.NewString()
	c := illustration.Confirmation{RequestID: id, VersionID: id, Prompt: "Synthetic tree", ConsentRevision: "2026-09-27"}
	_, err = runtime.create(owner, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := runtime.path(owner, id)
	g.before = func() {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		job, err := runtime.load(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		_ = job.Task.Cancel(job.Task.Revision)
		job.Input = nil
		_ = runtime.write(job)
	}
	runtime.process(context.Background(), path)
	job, err := runtime.load(owner, id)
	if err != nil || job.Task.State != illustration.TaskCancelled || job.Result != nil || job.Task.Charge != illustration.ChargeOneImage {
		t.Fatalf("late result restored %v %v", job, err)
	}
	id = uuid.NewString()
	c.RequestID = id
	c.VersionID = id
	_, err = runtime.create(owner, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, _ = runtime.load(owner, id)
	_ = job.Task.Start(job.Task.Revision, now)
	_ = runtime.write(job)
	restarted, err := NewIllustrationRuntime(root, token, g, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restarted.authorize = runtime.authorize
	job, _ = restarted.load(owner, id)
	if job.Task.State != illustration.TaskUncertain || job.Input != nil {
		t.Fatal("running recovery must remain uncertain")
	}
	restarted.process(context.Background(), restarted.path(owner, id))
	if g.calls.Load() != 1 {
		t.Fatal("ambiguous task redispatched")
	}
}

func TestIllustrationDisabledRouteReportsUnavailable(t *testing.T) {
	handler, err := New(Config{Token: strings.Repeat("i", 43), Organizer: &model{}, Speech: voice.ASR{}, Rewriter: voice.ArkRewriter{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", illustrationPrefix, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("i", 43))
	r.Header.Set("X-Journal-Development-Installation", uuid.NewString())
	r.Header.Set("X-Journal-Development-Mode", "forced")
	r.Header.Set("X-Journal-Development-Started-At", "2026-10-01T00:00:00Z")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not_ready") {
		t.Fatalf("disabled %d %s", w.Code, w.Body)
	}
}
