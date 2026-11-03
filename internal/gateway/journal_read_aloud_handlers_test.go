package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journalhttpapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type readAloudCatalogFixture struct {
	revision readaloud.CatalogRevision
	err      error
}

func (f *readAloudCatalogFixture) Current(context.Context) (readaloud.CatalogRevision, error) {
	return f.revision, f.err
}

type catalogSpeechFixture struct{}

func (catalogSpeechFixture) Synthesize(_ context.Context, _ readaloud.Synthesis, emit func([]byte) error) (readaloud.SynthesisUsage, error) {
	return readaloud.SynthesisUsage{AudioBytes: 2}, emit([]byte{1, 2})
}
func TestReadAloudCatalogPublicHTTPAndConditionalCache(t *testing.T) {
	voice := readaloud.Voice{ID: "synthetic_voice", Name: "测试声音", Languages: []string{"zh-cn"}, Styles: []string{"natural"}, ResourceID: readaloud.ResourceID, BillingClass: "standard", ProbeText: "专门编写的公开试听短句。", Enabled: true}
	voice, _, err := readaloud.VerifyVoice(context.Background(), voice, catalogSpeechFixture{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	candidate := voice
	candidate.ID = "candidate_voice"
	candidate.Enabled = false
	current := &readAloudCatalogFixture{revision: readaloud.CatalogRevision{Catalog: readaloud.Catalog{Version: uuid.NewString(), DefaultVoiceID: voice.ID, Voices: []readaloud.Voice{candidate, voice}}}}
	server := &Server{readAloudCatalog: current}
	router := gin.New()
	journalhttpapi.RegisterHandlers(router, journalhttpapi.NewStrictHandler(server, nil))
	call := func(tag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/journal/read-aloud/voices", nil)
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	response := call("")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Cache-Control"), "max-age=14400") || response.Header().Get("ETag") == "" {
		t.Fatal("missing cache contract")
	}
	var catalog readaloud.PublicCatalog
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil || len(catalog.Voices) != 1 || catalog.Voices[0].Dialects == nil {
		t.Fatal("public metadata shape invalid", err, catalog)
	}
	for _, private := range []string{"probeText", "verification", "resourceID", "billingClass", "candidate_voice"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatal("private/unpublished data leaked", private)
		}
	}
	if response := call("W/" + response.Header().Get("ETag")); response.Code != 304 || response.Body.Len() != 0 {
		t.Fatal("conditional request not honored", response.Code)
	}
	current.err = errors.New("private storage detail")
	if response := call(""); response.Code != 503 || strings.Contains(response.Body.String(), "private storage detail") {
		t.Fatal("unsafe catalog failure", response.Body.String())
	}
}
