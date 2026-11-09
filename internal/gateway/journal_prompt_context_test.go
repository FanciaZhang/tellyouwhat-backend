package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	journalprovider "github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

type frozenJournalOrganizer struct{ revision string }

func (o *frozenJournalOrganizer) Organize(ctx context.Context, _ journalcontracts.OrganizeRequest) (journalprovider.Result, error) {
	r, _ := promptconfig.FromContext(ctx)
	o.revision = r.ID
	return journalprovider.Result{InputTokens: 10, OutputTokens: 5}, nil
}

func TestJournalAnalysisPreservesHTTPRequestWithPublishedPrompt(t *testing.T) {
	for _, path := range []string{"/v1/journal/organization", "/v1/journal/illustration-suggestions"} {
		t.Run(path, func(t *testing.T) {
			cache := promptconfig.NewJournalCache(catalogFixture{})
			if err := cache.Refresh(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 2_000_000})
			organizer := &frozenJournalOrganizer{}
			server := New(Dependencies{
				App:           appregistry.App{ID: appregistry.Journal, AllowedOperationPrefix: "journal."},
				Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
				Quota: limiter, QuotaReader: limiter, Usage: usage.NewMemoryRecorder(), Media: newFakeMediaAuthorizer(),
				JournalOrganizer: organizer, JournalAnalysisVersion: "test", PromptConfig: cache,
			})
			body := map[string]any{"requestID": "19be2f9e-bd92-4699-b561-e3816092114c", "contentHash": strings.Repeat("a", 64), "title": "", "body": "今天在河边散步。"}
			if path == "/v1/journal/organization" {
				body["contractVersion"] = journalcontracts.ContractVersion
				body["existingTags"] = []string{}
				body["rejectedTagNames"] = []string{}
				body["books"] = []any{}
			}
			raw, _ := json.Marshal(body)
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, authorizedRequest(http.MethodPost, path, string(raw)))
			if response.Code != http.StatusOK || organizer.revision != "catalog-1" {
				t.Fatalf("status=%d revision=%q body=%s", response.Code, organizer.revision, response.Body)
			}
			replay := httptest.NewRecorder()
			server.Router().ServeHTTP(replay, authorizedRequest(http.MethodPost, path, string(raw)))
			if replay.Code != http.StatusConflict {
				t.Fatalf("request admission lost: %d %s", replay.Code, replay.Body)
			}
		})
	}
}
