package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journal/illustrationstore"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestMySQLJournalIllustrationResultStaysWithdrawnAfterNewConsent(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	if err := mysqlstore.NewKeyRepository(db, "journal").Register(ctx, attestation.RegisteredKey{
		KeyID: "valid-key", DeviceID: "device-1", PublicKey: []byte("synthetic"),
		Environment: "production", Receipt: []byte("synthetic"),
	}); err != nil {
		t.Fatal(err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := &illustrationstore.Store{Database: db, Cipher: cipher}
	consents := privacy.NewService(mysqlstore.NewJournalPrivacyRepository(db, store.Withdraw), nil, nil, time.Now)
	router := New(Dependencies{
		App: appregistry.App{ID: appregistry.Journal}, Authenticator: fakeAuthenticator{appID: "journal"},
		Entitlements: fakeEntitlements{allowed: true}, Privacy: consents, Consent: consents,
		AllowedConsentScopes: []string{privacy.ManagedAIScope, privacy.JournalIllustrationScope, privacy.JournalReadAloudScope},
		JournalIllustrations: &illustration.Service{Store: store, Generator: imageGeneratorFixture{}, Now: time.Now},
	}).Router()
	call := func(method, path, body string, status int, contains string) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authorizedRequest(method, path, body))
		if w.Code != status || !strings.Contains(w.Body.String(), contains) {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body.String())
		}
	}
	setConsent := func(granted bool) {
		t.Helper()
		body, err := json.Marshal(map[string][]privacy.Consent{"consents": privacy.JournalCloudAIConsents(granted)})
		if err != nil {
			t.Fatal(err)
		}
		call("POST", "/v1/privacy/consents", string(body), 200, "")
	}
	const id = "19be2f9e-bd92-4699-b561-e3816092114c"
	const route = "/v1/journal/illustrations"
	const body = `{"requestID":"` + id + `","versionID":"` + id + `","brief":{"title":"River","details":[{"kind":"subject","text":"A dog by the river"}]},"consentRevision":"` + privacy.JournalIllustrationDocumentVersion + `"}`
	setConsent(true)
	call("POST", route, body, 200, `"state":"queued"`)
	job, err := store.Get(ctx, "valid-key", id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := job.Task.Start(1, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, job, 1); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	result := &illustration.Result{Image: output.Bytes(), MIME: "image/png", Width: 12, Height: 8}
	if deliver, err := job.Task.RecordOutcome(2, result, nil, now); err != nil || !deliver {
		t.Fatal("could not prepare a completed image", err)
	}
	job.Input, job.Result = nil, result
	if err := store.Save(ctx, job, 2); err != nil {
		t.Fatal(err)
	}
	taskPath := route + "/" + id
	call("GET", taskPath+"/result", "", 200, base64.StdEncoding.EncodeToString(result.Image))
	setConsent(false)
	call("GET", taskPath+"/result", "", 403, "consent_required")
	setConsent(true)
	call("GET", taskPath, "", 200, `"state":"cancelled"`)
	call("GET", taskPath+"/result", "", 409, "image_task_changed")
	call("POST", route, body, 200, `"state":"cancelled"`)
}

// Explicit backend integration: real MySQL and configured image model, synthetic
// material and test authentication. This does not verify Apple device attestation.
func TestLiveJournalIllustrationConsentGenerationAndResultWithMySQL(t *testing.T) {
	if os.Getenv("JOURNAL_LIVE_IMAGE_CHECK") != "1" {
		t.Skip("explicit real-provider image integration only")
	}
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := mysqlstore.NewKeyRepository(db, "journal").Register(ctx, attestation.RegisteredKey{
		KeyID: "valid-key", DeviceID: "device-1", PublicKey: []byte("synthetic"),
		Environment: "production", Receipt: []byte("synthetic")}); err != nil {
		t.Fatal(err)
	}
	provider, err := illustration.NewProvider(os.Getenv("JOURNAL_IMAGE_BASE_URL"), os.Getenv("JOURNAL_IMAGE_API_KEY"), os.Getenv("JOURNAL_IMAGE_MODEL_ID"), nil)
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := strconv.ParseInt(os.Getenv("JOURNAL_IMAGE_MAX_COST_NANOS"), 10, 64)
	if err != nil || maximum <= 0 || maximum > 1_000_000_000 {
		t.Fatal("a bounded image cost is required")
	}
	budget, err := costcontrol.New(mysqlstore.NewCostControlStore(db), costcontrol.Limits{
		MonthlyBudgetNanos: maximum, MaxConcurrent: 1, LeaseDuration: 5 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := &illustrationstore.Store{Database: db, Cipher: cipher}
	consents := privacy.NewService(mysqlstore.NewJournalPrivacyRepository(db, store.Withdraw), nil, nil, time.Now)
	service := &illustration.Service{Store: store, Generator: &illustration.BudgetedGenerator{
		Next: provider, Controller: budget, MaxCostNanos: maximum}, Now: time.Now,
		Authorize: func(ctx context.Context, owner string) bool {
			ok, err := consents.HasRequiredConsents(ctx, Principal{AppID: "journal", KeyID: owner},
				[]string{privacy.ManagedAIScope, privacy.JournalIllustrationScope})
			return err == nil && ok
		}}
	server := New(Dependencies{App: appregistry.App{ID: appregistry.Journal},
		Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
		Privacy: consents, Consent: consents, AllowedConsentScopes: []string{privacy.ManagedAIScope, privacy.JournalIllustrationScope, privacy.JournalReadAloudScope},
		JournalIllustrations: service})
	router := server.Router()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, authorizedRequest(method, path, body))
		if w.Code != 200 {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	call("POST", "/v1/privacy/consents", `{"consents":[{"scope":"managed_subscription","documentVersion":"2026-11-03","granted":true},{"scope":"journal_illustration","documentVersion":"2026-11-03","granted":true},{"scope":"journal_read_aloud","documentVersion":"2026-11-03","granted":true}]}`)
	const id = "19be2f9e-bd92-4699-b561-e3816092114c"
	const body = `{"requestID":"` + id + `","versionID":"` + id + `","brief":{"title":"河岸边的小狗","details":[{"kind":"subject","text":"一只小狗在傍晚的河岸散步，柔和水彩插画，不绘制文字。"}]},"consentRevision":"2026-11-03"}`
	call("POST", "/v1/journal/illustrations", body)
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		w := call("GET", "/v1/journal/illustrations/"+id, "")
		var task struct{ State, Problem string }
		if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.State == "succeeded" {
			break
		}
		if task.State != "queued" && task.State != "running" {
			t.Fatalf("image generation ended in %s (%s)", task.State, task.Problem)
		}
		select {
		case <-ctx.Done():
			t.Fatal("image generation did not finish")
		case <-time.After(time.Second):
		}
	}
	w := call("GET", "/v1/journal/illustrations/"+id+"/result", "")
	var result struct {
		Image         []byte
		MIME          string
		Width, Height int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(result.Image))
	if err != nil || decoded.Bounds().Dx() != result.Width || decoded.Bounds().Dy() != result.Height {
		t.Fatal("returned image does not decode with its advertised dimensions", err)
	}
	// Recreate the store, and replay admission, without losing or regenerating the result.
	reopened := illustrationstore.Store{Database: db, Cipher: cipher}
	job, err := reopened.Get(ctx, "valid-key", id)
	if err != nil || job.Result == nil || !bytes.Equal(job.Result.Image, result.Image) {
		t.Fatal("stored original did not survive reopening", err)
	}
	call("POST", "/v1/journal/illustrations", body)
	if output := os.Getenv("JOURNAL_LIVE_IMAGE_OUTPUT"); output != "" {
		if err := os.WriteFile(output, result.Image, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("real configured image generated, decoded and persisted: %dx%d %s bytes=%d sha256=%x",
		result.Width, result.Height, result.MIME, len(result.Image), sha256.Sum256(result.Image))
}
