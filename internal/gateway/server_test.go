package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/jobs"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	journalprovider "github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	quotaapi "github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

func TestAIRequestRequiresAssertion(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	request := httptest.NewRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", strings.NewReader(validBody()))
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", response.Code, response.Body.String())
	}
}

func TestJournalOrganizeUsesServerOwnedOperationAndReturnsTagsAndBooks(t *testing.T) {
	t.Parallel()
	limiter := quotaapi.NewMemoryLimiter(quotaapi.Limits{
		DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 2_000_000,
	})
	organizer := &fakeJournalOrganizer{result: journalprovider.Result{
		Value: journalcontracts.ModelResult{
			Tags:                        []journalcontracts.Tag{{Name: "西湖", Type: "place"}},
			ExistingBookRecommendations: []journalcontracts.ExistingBookRecommendation{{BookID: "8d43cd74-5652-4412-b097-303f563e673a", Reason: "记录杭州生活"}},
		},
		InputTokens: 120, OutputTokens: 30,
	}}
	server := New(Dependencies{
		App: appregistry.App{
			ID: appregistry.Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"},
			TeamID: "TEAM", BundleID: "cn.tellyouwhat.journalapp",
			ManagedAIProductID: "journal.ai.subscription.monthly", AllowedOperationPrefix: "journal.",
		},
		Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
		Quota: limiter, QuotaReader: limiter, Usage: usage.NewMemoryRecorder(),
		Media:            newFakeMediaAuthorizer(),
		JournalOrganizer: organizer, JournalAnalysisVersion: "journal-organize-test",
		Consent: fakeConsentGate{granted: true}, RequiredConsentScopes: []string{privacy.ManagedAIScope},
		Privacy: &fakePrivacyManager{}, Readiness: ReadinessFunc(func(context.Context) error { return nil }),
	})
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","contractVersion":"journal-organize-v1","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"周末","body":"今天去了西湖","existingTags":[],"rejectedTagNames":[],"books":[{"id":"8d43cd74-5652-4412-b097-303f563e673a","name":"杭州","description":"城市生活","containsEntry":false}]}`
	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/operations/journal.organize/responses", body))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"西湖"`) ||
		!strings.Contains(response.Body.String(), `"bookID":"8d43cd74-5652-4412-b097-303f563e673a"`) ||
		!strings.Contains(response.Body.String(), `"dailyTokensRemaining"`) {
		t.Fatalf("unexpected journal response: %d %s", response.Code, response.Body.String())
	}
	if organizer.request.Body != "今天去了西湖" {
		t.Fatalf("journal input did not reach fixed organizer: %+v", organizer.request)
	}
}

func TestJournalOrganizeRejectsMissingConsentBeforeProviderCall(t *testing.T) {
	t.Parallel()
	limiter := quotaapi.NewMemoryLimiter(quotaapi.Limits{DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 2_000_000})
	organizer := &fakeJournalOrganizer{}
	server := New(Dependencies{
		App:           appregistry.App{ID: appregistry.Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"}, TeamID: "TEAM", BundleID: "journal.bundle", ManagedAIProductID: "journal.ai.monthly", AllowedOperationPrefix: "journal."},
		Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
		Quota: limiter, QuotaReader: limiter, Usage: usage.NewMemoryRecorder(), Media: newFakeMediaAuthorizer(), JournalOrganizer: organizer,
		Consent: fakeConsentGate{granted: false}, RequiredConsentScopes: []string{privacy.ManagedAIScope},
		Privacy: &fakePrivacyManager{}, Readiness: ReadinessFunc(func(context.Context) error { return nil }),
	})
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","contractVersion":"journal-organize-v1","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"周末","body":"正文","existingTags":[],"rejectedTagNames":[],"books":[]}`
	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/operations/journal.organize/responses", body))
	if response.Code != http.StatusForbidden || organizer.calls != 0 {
		t.Fatalf("consent was not enforced: %d %s calls=%d", response.Code, response.Body.String(), organizer.calls)
	}
}

func TestJournalOrganizeRejectsIdempotentReplayBeforeSecondProviderCall(t *testing.T) {
	t.Parallel()
	limiter := quotaapi.NewMemoryLimiter(quotaapi.Limits{
		DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 2_000_000,
	})
	organizer := &fakeJournalOrganizer{result: journalprovider.Result{
		Value: journalcontracts.ModelResult{}, InputTokens: 20, OutputTokens: 5,
	}}
	server := New(Dependencies{
		App: appregistry.App{
			ID: appregistry.Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"},
			TeamID: "TEAM", BundleID: "cn.tellyouwhat.journalapp",
			ManagedAIProductID: "journal.ai.subscription.monthly", AllowedOperationPrefix: "journal.",
		},
		Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
		Quota: limiter, QuotaReader: limiter, Usage: usage.NewMemoryRecorder(), Media: newFakeMediaAuthorizer(),
		JournalOrganizer: organizer, JournalAnalysisVersion: "journal-organize-test",
		Consent: fakeConsentGate{granted: true}, RequiredConsentScopes: []string{privacy.ManagedAIScope},
		Privacy: &fakePrivacyManager{}, Readiness: ReadinessFunc(func(context.Context) error { return nil }),
	})
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","contractVersion":"journal-organize-v1","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"周末","body":"正文","existingTags":[],"rejectedTagNames":[],"books":[]}`
	first := httptest.NewRecorder()
	server.ServeHTTP(first, authorizedRequest(http.MethodPost, "/v1/ai/operations/journal.organize/responses", body))
	second := httptest.NewRecorder()
	server.ServeHTTP(second, authorizedRequest(http.MethodPost, "/v1/ai/operations/journal.organize/responses", body))
	if first.Code != http.StatusOK || second.Code != http.StatusConflict || organizer.calls != 1 {
		t.Fatalf("journal request was not idempotently committed: first=%d second=%d calls=%d body=%s", first.Code, second.Code, organizer.calls, second.Body.String())
	}
}

func TestJournalOrganizeReturnsResultWhenQuotaSnapshotIsUnavailable(t *testing.T) {
	t.Parallel()
	limiter := quotaapi.NewMemoryLimiter(quotaapi.Limits{
		DailyTokensPerTransaction: 1_000_000, MonthlyTokensPerTransaction: 2_000_000,
	})
	organizer := &fakeJournalOrganizer{result: journalprovider.Result{
		Value: journalcontracts.ModelResult{
			Tags: []journalcontracts.Tag{{Name: "西湖", Type: "place"}},
		},
		InputTokens: 20, OutputTokens: 5,
	}}
	server := New(Dependencies{
		App: appregistry.App{
			ID: appregistry.Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"},
			TeamID: "TEAM", BundleID: "cn.tellyouwhat.journalapp",
			ManagedAIProductID: "journal.ai.subscription.monthly", AllowedOperationPrefix: "journal.",
		},
		Authenticator: fakeAuthenticator{appID: "journal"}, Entitlements: fakeEntitlements{allowed: true},
		Quota: limiter, QuotaReader: failingQuotaReader{}, Usage: usage.NewMemoryRecorder(), Media: newFakeMediaAuthorizer(),
		JournalOrganizer: organizer, JournalAnalysisVersion: "journal-organize-test",
		Consent: fakeConsentGate{granted: true}, RequiredConsentScopes: []string{privacy.ManagedAIScope},
		Privacy: &fakePrivacyManager{}, Readiness: ReadinessFunc(func(context.Context) error { return nil }),
	})
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","contractVersion":"journal-organize-v1","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"周末","body":"正文","existingTags":[],"rejectedTagNames":[],"books":[]}`
	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/operations/journal.organize/responses", body))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"西湖"`) ||
		!strings.Contains(response.Body.String(), `"available":false`) {
		t.Fatalf("successful model result was discarded after quota snapshot failure: %d %s", response.Code, response.Body.String())
	}
}

func TestRegistrationConflictsHaveDistinctErrorCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "used challenge", err: attestation.ErrReplay, code: "replay_detected"},
		{name: "registered key", err: attestation.ErrKeyAlreadyRegistered, code: "key_already_registered"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := New(Dependencies{Enrollment: fakeEnrollment{registerErr: test.err}})
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost,
				"/v1/attest/keys",
				strings.NewReader(`{"keyID":"key","challenge":"challenge","attestation":"attestation","build":"1"}`),
			))
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("unexpected registration conflict: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestQuotaStatusRequiresAssertionAndReturnsSubscriptionLimits(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/ai/quota", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodGet, "/v1/ai/quota", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"dailyLimit":1000000000`) ||
		!strings.Contains(response.Body.String(), `"monthlyLimit":2000000000`) {
		t.Fatalf("unexpected quota snapshot: %s", response.Body.String())
	}
}

func TestManagedProductPublishesLimitsAndLegalLinksWithoutAuthentication(t *testing.T) {
	t.Parallel()
	server := newTestServer()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/products/managed-ai", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"productID":"health.ai.subscription.monthly"`) ||
		!strings.Contains(response.Body.String(), `"privacyURL":"https://health.tellyouwhat.cn/privacy"`) {
		t.Fatalf("unexpected product response: %d %s", response.Code, response.Body.String())
	}
}

func TestPrivacyEndpointsRequireAssertionAndUseAttestedPrincipal(t *testing.T) {
	t.Parallel()
	server := newTestServer()
	manager := &fakePrivacyManager{}
	server.privacy = manager

	consent := httptest.NewRecorder()
	server.ServeHTTP(consent, authorizedRequest(http.MethodPost, "/v1/privacy/consents",
		`{"consents":[{"scope":"managed_subscription","documentVersion":"2026-08-24","granted":true}]}`))
	if consent.Code != http.StatusOK || manager.principal.KeyID != "valid-key" || len(manager.consents) != 1 {
		t.Fatalf("unexpected consent result: %d %s manager=%+v", consent.Code, consent.Body.String(), manager)
	}

	deleted := httptest.NewRecorder()
	server.ServeHTTP(deleted, authorizedRequest(http.MethodDelete, "/v1/privacy/data", ""))
	if deleted.Code != http.StatusNoContent || !manager.deleted {
		t.Fatalf("unexpected deletion result: %d %s manager=%+v", deleted.Code, deleted.Body.String(), manager)
	}
}

func TestProductionEntitlementSyncRequiresAssertionAndVerifiedTransaction(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	syncer := &fakeProductionEntitlementSync{}
	server.productionEntitlement = syncer
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(
		http.MethodPost,
		"/v1/entitlements/transactions",
		strings.NewReader(`{"signedTransaction":"signed-transaction"}`),
	))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", unauthorized.Code, unauthorized.Body.String())
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(
		http.MethodPost,
		"/v1/entitlements/transactions",
		`{"signedTransaction":"signed-transaction"}`,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if syncer.signedTransaction != "signed-transaction" || syncer.principal.KeyID != "valid-key" {
		t.Fatalf("transaction was not bound to the attested principal: %+v", syncer)
	}
}

func TestAppStoreNotificationAcceptsOnlyVerifiedProcessorResult(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	processor := &fakeAppStoreNotificationProcessor{}
	server.appStoreNotifications = processor
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/app-store/notifications",
		strings.NewReader(`{"signedPayload":"signed-notification"}`),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if processor.signedPayload != "signed-notification" {
		t.Fatalf("notification payload was not processed: %+v", processor)
	}
}

func TestAppStoreNotificationRejectsMalformedEnvelopeAsBadRequest(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.appStoreNotifications = &fakeAppStoreNotificationProcessor{}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/v1/app-store/notifications",
		strings.NewReader(`{"signedPayload":`),
	))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestMapsAttestationInfrastructureFailureToServiceUnavailable(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.authenticator = fakeAuthenticator{err: attestation.ErrUnavailable}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestRejectsUnknownVersionAsUpgradeRequired(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	body := strings.Replace(validBody(), "meal-decision-v10-fresh-exploration", "meal-decision-v999", 1)
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", body)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("expected 426, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestRejectsClientSelectedModel(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	body := strings.Replace(validBody(), `"prompt":"choose dinner"`, `"prompt":"choose dinner","model":"stolen"`, 1)
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", body)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestRejectsSchemaOutsideServerManifest(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.contracts = rejectingContractValidator{}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestChecksManagedEntitlement(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.entitlements = fakeEntitlements{allowed: false}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIRequestEnforcesQuotaBeforeCallingArk(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{}
	server := newTestServer()
	server.provider = provider
	server.quota = fakeQuota{err: ErrQuotaExceeded}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", response.Code, response.Body.String())
	}
	if provider.completeCalls != 0 {
		t.Fatalf("provider should not be called after quota denial")
	}
}

func TestAIRequestExplainsDailySafetyLimit(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.quota = fakeQuota{err: quotaapi.Exceeded(quotaapi.LimitDailyTokens)}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests ||
		!strings.Contains(response.Body.String(), `"code":"daily_quota_exceeded"`) {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
}

func TestAIRequestForwardsOnlyValidatedArtifactToFixedProvider(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{}
	server := newTestServer()
	server.provider = provider
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if provider.completeCalls != 1 || provider.lastRequest.Prompt != "choose dinner" {
		t.Fatalf("provider did not receive the validated artifact")
	}
	if !strings.Contains(response.Body.String(), `"content":"fixed result"`) {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}

func TestAIRequestDeliversCompletedResultWhenUsageLedgerIsUnavailable(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.usage = failingUsageRecorder{}
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"content":"fixed result"`) {
		t.Fatalf("billable completed result was discarded: %d %s", response.Code, response.Body.String())
	}
}

func TestAIRequestRejectsRepeatedRequestIDBeforeSecondProviderCost(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{}
	server := newTestServer()
	server.provider = provider
	first := httptest.NewRecorder()
	server.ServeHTTP(first, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody()))
	second := httptest.NewRecorder()
	server.ServeHTTP(second, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", validBody()))

	if first.Code != http.StatusOK || second.Code != http.StatusConflict {
		t.Fatalf("unexpected statuses: first=%d second=%d (%s)", first.Code, second.Code, second.Body.String())
	}
	if provider.completeCalls != 1 {
		t.Fatalf("replay incurred a second provider call: %d", provider.completeCalls)
	}
}

func TestAIRequestRejectsMediaOwnedByAnotherDevice(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	body := strings.Replace(validPhotoBody(), "ai-temp/health/device-1/", "ai-temp/stolen-device/", 1)
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/responses", body)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAIStreamUsesSSEAndFlushesDeltas(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/streams", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("unexpected content type: %q", got)
	}
	if !strings.Contains(response.Body.String(), "event: delta\n") || !strings.Contains(response.Body.String(), "event: completed\n") {
		t.Fatalf("missing SSE events: %s", response.Body.String())
	}
}

func TestBackgroundJobDispatchesOnlyServerJobID(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	jobService := &fakeJobService{}
	dispatcher := &fakeDispatcher{}
	server.jobs = jobService
	server.dispatcher = dispatcher
	request := authorizedRequest(http.MethodPost, "/v1/ai/jobs", validBody())
	request.Header.Set("Authorization", "JobCapability valid-token")
	request.Header.Set("X-Tellyouwhat-Job-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", response.Code, response.Body.String())
	}
	if dispatcher.jobID != "19be2f9e-bd92-4699-b561-e3816092114c" || jobService.lastRequest.Prompt != "choose dinner" {
		t.Fatalf("job was not constrained and dispatched: job=%q request=%+v", dispatcher.jobID, jobService.lastRequest)
	}
}

func TestBackgroundJobCapabilitySurvivesTransientJobInsertFailure(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	jobService := &fakeJobService{enqueueErr: errors.New("database unavailable")}
	capabilities := &fakeCapabilities{}
	server.jobs = jobService
	server.dispatcher = &fakeDispatcher{}
	server.capabilities = capabilities

	first := authorizedRequest(http.MethodPost, "/v1/ai/jobs", validBody())
	first.Header.Set("Authorization", "JobCapability valid-token")
	first.Header.Set("X-Tellyouwhat-Job-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
	firstResponse := httptest.NewRecorder()
	server.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected retryable 503, got %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	if capabilities.consumeCalls != 0 {
		t.Fatalf("capability was burned before durable insert: %d", capabilities.consumeCalls)
	}

	second := authorizedRequest(http.MethodPost, "/v1/ai/jobs", validBody())
	second.Header.Set("Authorization", "JobCapability valid-token")
	second.Header.Set("X-Tellyouwhat-Job-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
	secondResponse := httptest.NewRecorder()
	server.ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusAccepted {
		t.Fatalf("retry should be accepted, got %d: %s", secondResponse.Code, secondResponse.Body.String())
	}
	if capabilities.consumeCalls != 1 {
		t.Fatalf("capability should be consumed after durable insert: %d", capabilities.consumeCalls)
	}

	replay := authorizedRequest(http.MethodPost, "/v1/ai/jobs", validBody())
	replay.Header.Set("Authorization", "JobCapability valid-token")
	replay.Header.Set("X-Tellyouwhat-Job-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
	replayResponse := httptest.NewRecorder()
	server.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusAccepted {
		t.Fatalf("same signed job replay must return the durable job, got %d: %s", replayResponse.Code, replayResponse.Body.String())
	}
}

func TestBackgroundJobCapabilityRequiresAppAttestAndBindsArtifact(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.jobs = &fakeJobService{}
	server.dispatcher = &fakeDispatcher{}
	capabilities := &fakeCapabilities{}
	server.capabilities = capabilities
	request := authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/job-capabilities", validBody())
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", response.Code, response.Body.String())
	}
	if capabilities.issuedBinding.Operation != contracts.OperationMealDecision || capabilities.issuedBinding.BodyDigest == "" || capabilities.issuedBinding.MediaDigest == "" {
		t.Fatalf("capability did not bind the validated artifact: %+v", capabilities.issuedBinding)
	}
}

func TestBackgroundJobCapabilityCanBeReissuedAfterLostResponse(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.quota = quotaapi.NewMemoryLimiter(quotaapi.Limits{
		RequestsPerMinutePerDevice: 1,
		DailyTokensPerTransaction:  100_000,
		MaxConcurrentPerDevice:     1,
	})
	server.jobs = &fakeJobService{}
	server.dispatcher = &fakeDispatcher{}
	capabilities := &fakeCapabilities{}
	server.capabilities = capabilities
	first := httptest.NewRecorder()
	server.ServeHTTP(first, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/job-capabilities", validBody()))
	second := httptest.NewRecorder()
	server.ServeHTTP(second, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/job-capabilities", validBody()))
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() {
		t.Fatalf("lost capability response was not idempotently recoverable: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}

func TestCapabilityMediaReplayCannotRefundOwnedQuotaReservation(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	lease := &recordingQuotaLease{}
	server.quota = recordingQuota{lease: lease}
	server.jobs = &fakeJobService{}
	server.dispatcher = &fakeDispatcher{}
	server.capabilities = &fakeCapabilities{}
	authorizer := newFakeMediaAuthorizer()
	body := validBody()
	authorizer.attempts["19be2f9e-bd92-4699-b561-e3816092114c"] = contracts.BodySHA256([]byte(body))
	server.media = authorizer
	response := httptest.NewRecorder()

	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/job-capabilities", body))

	if response.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", response.Code, response.Body.String())
	}
	if lease.actualTokens <= 0 {
		t.Fatalf("an owned quota reservation was refunded after a concurrent media replay: %d", lease.actualTokens)
	}
}

func TestCapabilityAdmissionFailureRetainsReservationForConcurrentRetry(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	lease := &recordingQuotaLease{}
	server.quota = recordingQuota{lease: lease}
	server.jobs = &fakeJobService{}
	server.dispatcher = &fakeDispatcher{}
	server.capabilities = &fakeCapabilities{}
	authorizer := newFakeMediaAuthorizer()
	authorizer.admitErr = media.ErrUnavailable
	server.media = authorizer
	response := httptest.NewRecorder()

	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/ai/operations/health.meal.decision/job-capabilities", validBody()))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", response.Code, response.Body.String())
	}
	if lease.actualTokens <= 0 {
		t.Fatalf("an owned quota reservation was refunded while a concurrent retry could still admit: %d", lease.actualTokens)
	}
}

func TestMediaAuthorizationRequiresManagedEntitlement(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.media = newFakeMediaAuthorizer()
	server.entitlements = fakeEntitlements{allowed: false}
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","operation":"health.meal.photo-capture","mediaID":"photo-1","kind":"image","mimeType":"image/jpeg","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1024}`
	request := authorizedRequest(http.MethodPost, "/v1/media/upload-authorizations", body)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", response.Code, response.Body.String())
	}
}

func TestMediaAuthorizationMapsStorageOrPresignFailureToServiceUnavailable(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	mediaService := newFakeMediaAuthorizer()
	mediaService.authorizeErr = errors.New("tos unavailable")
	server.media = mediaService
	body := `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c","operation":"health.meal.photo-capture","mediaID":"photo-1","kind":"image","mimeType":"image/jpeg","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1024}`
	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodPost, "/v1/media/upload-authorizations", body))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", response.Code, response.Body.String())
	}
}

func TestJobLookupMapsStorageFailureToServiceUnavailable(t *testing.T) {
	t.Parallel()

	server := newTestServer()
	server.jobs = &fakeJobService{getErr: errors.New("database unavailable")}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, authorizedRequest(http.MethodGet, "/v1/ai/jobs/19be2f9e-bd92-4699-b561-e3816092114c", ""))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", response.Code, response.Body.String())
	}
}

func newTestServer() *Server {
	limiter := quotaapi.NewMemoryLimiter(quotaapi.Limits{
		DailyTokensPerTransaction:   1_000_000_000,
		MonthlyTokensPerTransaction: 2_000_000_000,
	})
	return New(Dependencies{
		Authenticator: fakeAuthenticator{},
		Entitlements:  fakeEntitlements{allowed: true},
		Quota:         limiter,
		QuotaReader:   limiter,
		Provider:      &fakeProvider{},
		Capabilities:  &fakeCapabilities{},
		Contracts:     allowingContractValidator{},
		Media:         newFakeMediaAuthorizer(),
		Usage:         usage.NewMemoryRecorder(),
		Readiness:     ReadinessFunc(func(context.Context) error { return nil }),
		Privacy:       &fakePrivacyManager{},
		ManagedProduct: ManagedProduct{
			ProductID:  "health.ai.subscription.monthly",
			PrivacyURL: "https://health.tellyouwhat.cn/privacy",
		},
	})
}

func authorizedRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-Tellyouwhat-Key-ID", "valid-key")
	request.Header.Set("X-Tellyouwhat-Assertion", "valid-assertion")
	request.Header.Set("X-Tellyouwhat-Nonce", "valid-nonce")
	request.Header.Set("X-Tellyouwhat-Timestamp", "2026-08-02T08:00:00Z")
	request.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
	return request
}

func validBody() string {
	return `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c",` +
		`"operation":"health.meal.decision",` +
		`"contractVersion":"ai-request-v1",` +
		`"promptVersion":"meal-decision-v10-fresh-exploration",` +
		`"prompt":"choose dinner",` +
		`"responseSchema":{"type":"object","additionalProperties":false},` +
		`"options":{},` +
		`"media":[],` +
		`"semanticSignature":"sha256:abc"}`
}

func validPhotoBody() string {
	return `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c",` +
		`"operation":"health.meal.photo-capture",` +
		`"contractVersion":"ai-request-v1",` +
		`"promptVersion":"meal-photo-v4",` +
		`"prompt":"identify dinner",` +
		`"responseSchema":{"type":"object","additionalProperties":false},` +
		`"options":{},` +
		`"media":[{"id":"photo-1","kind":"image","mimeType":"image/jpeg",` +
		`"objectID":"ai-temp/health/device-1/19be2f9e-bd92-4699-b561-e3816092114c/photo-1",` +
		`"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sizeBytes":1024}],` +
		`"semanticSignature":"sha256:abc"}`
}

type fakeAuthenticator struct {
	err   error
	appID string
}

type allowingContractValidator struct{}

func (allowingContractValidator) Validate(contracts.Request) error { return nil }

type rejectingContractValidator struct{}

func (rejectingContractValidator) Validate(contracts.Request) error {
	return contracts.ErrContractViolation
}

func (authenticator fakeAuthenticator) Authenticate(_ context.Context, proof RequestProof) (Principal, error) {
	if authenticator.err != nil {
		return Principal{AppID: "health"}, authenticator.err
	}
	if proof.KeyID == "" || proof.Assertion == "" || proof.Nonce == "" {
		return Principal{AppID: "health"}, ErrAuthentication
	}
	if proof.RequestID != "19be2f9e-bd92-4699-b561-e3816092114c" {
		return Principal{AppID: "health"}, ErrAuthentication
	}
	appID := authenticator.appID
	if appID == "" {
		appID = "health"
	}
	return Principal{AppID: appID, KeyID: proof.KeyID, DeviceID: "device-1", TransactionID: "transaction-1"}, nil
}

type fakeEntitlements struct{ allowed bool }

type fakePrivacyManager struct {
	principal Principal
	consents  []privacy.Consent
	deleted   bool
}

type fakeConsentGate struct {
	granted bool
	err     error
}

func (gate fakeConsentGate) HasRequiredConsents(context.Context, Principal, []string) (bool, error) {
	return gate.granted, gate.err
}

type fakeJournalOrganizer struct {
	request journalcontracts.OrganizeRequest
	result  journalprovider.Result
	err     error
	calls   int
}

func (organizer *fakeJournalOrganizer) Organize(_ context.Context, request journalcontracts.OrganizeRequest) (journalprovider.Result, error) {
	organizer.calls++
	organizer.request = request
	return organizer.result, organizer.err
}

func (manager *fakePrivacyManager) RecordConsents(_ context.Context, principal Principal, consents []privacy.Consent) (time.Time, error) {
	manager.principal = principal
	manager.consents = consents
	return time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC), nil
}

func (manager *fakePrivacyManager) DeletePrincipal(_ context.Context, principal Principal) error {
	manager.principal = principal
	manager.deleted = true
	return nil
}

type fakeProductionEntitlementSync struct {
	principal         Principal
	signedTransaction string
}

type fakeAppStoreNotificationProcessor struct {
	signedPayload string
}

func (processor *fakeAppStoreNotificationProcessor) Process(
	_ context.Context,
	signedPayload string,
) (bool, error) {
	processor.signedPayload = signedPayload
	return true, nil
}

func (syncer *fakeProductionEntitlementSync) Sync(
	_ context.Context,
	principal Principal,
	signedTransaction string,
) (time.Time, error) {
	syncer.principal = principal
	syncer.signedTransaction = signedTransaction
	return time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC), nil
}

func (entitlement fakeEntitlements) HasManagedSubscription(context.Context, Principal) (bool, error) {
	return entitlement.allowed, nil
}

type fakeQuota struct{ err error }

type failingQuotaReader struct{}

func (failingQuotaReader) Snapshot(context.Context, string, time.Time) (quotaapi.Snapshot, error) {
	return quotaapi.Snapshot{}, errors.New("snapshot unavailable")
}

type fakeEnrollment struct{ registerErr error }

func (fakeEnrollment) IssueChallenge(context.Context, string) (attestation.Challenge, error) {
	return attestation.Challenge{}, nil
}

func (enrollment fakeEnrollment) Register(context.Context, attestation.RegistrationRequest) (Principal, error) {
	return Principal{}, enrollment.registerErr
}

func (value fakeQuota) Acquire(
	context.Context,
	quotaapi.Identity,
	contracts.Operation,
	int,
	string,
	time.Time,
) (quotaapi.Releaser, error) {
	if value.err != nil {
		return nil, value.err
	}
	return fakeQuotaLease{}, nil
}

type fakeQuotaLease struct{}

func (fakeQuotaLease) Release(int) {}

type recordingQuota struct{ lease *recordingQuotaLease }

func (value recordingQuota) Acquire(
	context.Context,
	quotaapi.Identity,
	contracts.Operation,
	int,
	string,
	time.Time,
) (quotaapi.Releaser, error) {
	return value.lease, nil
}

type recordingQuotaLease struct{ actualTokens int }

func (lease *recordingQuotaLease) Release(actualTokens int) { lease.actualTokens = actualTokens }

type fakeProvider struct {
	completeCalls int
	completeErr   error
	lastRequest   contracts.Request
}

type fakeJobService struct {
	lastRequest contracts.Request
	enqueueErr  error
	getErr      error
	cancelErr   error
}

func (service *fakeJobService) Enqueue(_ context.Context, _ Principal, request contracts.Request, _ string) (jobs.Job, error) {
	service.lastRequest = request
	return jobs.Job{ID: "job-1", RequestID: request.RequestID, Status: jobs.StatusQueued}, nil
}

func (service *fakeJobService) EnqueueWithID(_ context.Context, _ Principal, jobID string, request contracts.Request, _ string) (jobs.Job, error) {
	service.lastRequest = request
	if service.enqueueErr != nil {
		err := service.enqueueErr
		service.enqueueErr = nil
		return jobs.Job{}, err
	}
	return jobs.Job{ID: jobID, RequestID: request.RequestID, Status: jobs.StatusQueued}, nil
}

func (service *fakeJobService) Get(context.Context, Principal, string) (jobs.Job, error) {
	if service.getErr != nil {
		return jobs.Job{}, service.getErr
	}
	return jobs.Job{}, jobs.ErrNotFound
}

func (service *fakeJobService) Cancel(context.Context, Principal, string) error {
	if service.cancelErr != nil {
		return service.cancelErr
	}
	return jobs.ErrNotFound
}

type fakeDispatcher struct{ jobID string }

func (dispatcher *fakeDispatcher) Dispatch(_ context.Context, jobID string) error {
	dispatcher.jobID = jobID
	return nil
}

type fakeCapabilities struct {
	issuedBinding capability.Binding
	consumeCalls  int
	consumed      bool
}

func (service *fakeCapabilities) IssueAt(_ Principal, binding capability.Binding, issuedAt time.Time) (capability.Issued, error) {
	service.issuedBinding = binding
	return capability.Issued{JobID: "19be2f9e-bd92-4699-b561-e3816092114c", Token: "valid-token", ExpiresAt: issuedAt.Add(time.Hour)}, nil
}

func (service *fakeCapabilities) Consume(_ context.Context, token string, binding capability.Binding) (Principal, error) {
	if token != "valid-token" || binding.JobID == "" {
		return Principal{AppID: "health"}, capability.ErrInvalid
	}
	service.consumeCalls++
	if service.consumed {
		return Principal{AppID: "health"}, capability.ErrReplay
	}
	service.consumed = true
	return Principal{AppID: "health", KeyID: "valid-key", DeviceID: "device-1", TransactionID: "transaction-1"}, nil
}

func (*fakeCapabilities) Validate(token string, binding capability.Binding) (Principal, error) {
	if token != "valid-token" || binding.JobID == "" {
		return Principal{AppID: "health"}, capability.ErrInvalid
	}
	return Principal{AppID: "health", KeyID: "valid-key", DeviceID: "device-1", TransactionID: "transaction-1"}, nil
}

type fakeMediaAuthorizer struct {
	mu           sync.Mutex
	attempts     map[string]string
	authorizeErr error
	admitErr     error
}

func newFakeMediaAuthorizer() *fakeMediaAuthorizer {
	return &fakeMediaAuthorizer{attempts: make(map[string]string)}
}

func (authorizer *fakeMediaAuthorizer) Authorize(context.Context, attestation.Principal, media.UploadRequest) (media.UploadAuthorization, error) {
	if authorizer.authorizeErr != nil {
		return media.UploadAuthorization{}, authorizer.authorizeErr
	}
	return media.UploadAuthorization{}, nil
}

type failingUsageRecorder struct{}

func (failingUsageRecorder) Record(context.Context, usage.Record) error {
	return errors.New("usage ledger unavailable")
}

func (authorizer *fakeMediaAuthorizer) Consume(_ context.Context, principal attestation.Principal, request contracts.Request, bodyDigest string) error {
	_, replay, err := authorizer.Admit(context.Background(), principal, request, bodyDigest)
	if err != nil {
		return err
	}
	if replay {
		return media.ErrIdempotencyReplay
	}
	return nil
}

func (authorizer *fakeMediaAuthorizer) Admit(_ context.Context, principal attestation.Principal, request contracts.Request, bodyDigest string) (media.AttemptRecord, bool, error) {
	if authorizer.admitErr != nil {
		return media.AttemptRecord{}, false, authorizer.admitErr
	}
	if err := authorizer.Validate(context.Background(), principal, request); err != nil {
		return media.AttemptRecord{}, false, err
	}
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	if existing, exists := authorizer.attempts[request.RequestID]; exists {
		if existing == bodyDigest {
			return media.AttemptRecord{
				RequestID: request.RequestID, OwnerKeyID: principal.KeyID, BodyDigest: bodyDigest,
				CreatedAt: time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC),
			}, true, nil
		}
		return media.AttemptRecord{}, false, media.ErrIdempotencyConflict
	}
	authorizer.attempts[request.RequestID] = bodyDigest
	return media.AttemptRecord{
		RequestID: request.RequestID, OwnerKeyID: principal.KeyID, BodyDigest: bodyDigest,
		CreatedAt: time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC),
	}, false, nil
}

func (*fakeMediaAuthorizer) Validate(_ context.Context, principal attestation.Principal, request contracts.Request) error {
	for _, item := range request.Media {
		if item.ObjectID != "ai-temp/"+principal.DeviceID+"/"+request.RequestID+"/"+item.ID {
			return media.ErrNotAuthorized
		}
	}
	return nil
}

func (provider *fakeProvider) Complete(_ context.Context, request contracts.Request) (ProviderResponse, error) {
	provider.completeCalls++
	provider.lastRequest = request
	if provider.completeErr != nil {
		return ProviderResponse{}, provider.completeErr
	}
	return ProviderResponse{Content: "fixed result", InputTokens: 10, OutputTokens: 4}, nil
}

func (provider *fakeProvider) Stream(_ context.Context, request contracts.Request, yield func(StreamEvent) error) error {
	provider.lastRequest = request
	if err := yield(StreamEvent{Delta: "fixed "}); err != nil {
		return err
	}
	if err := yield(StreamEvent{Delta: "result"}); err != nil {
		return err
	}
	return yield(StreamEvent{Completed: &ProviderResponse{Content: "fixed result", InputTokens: 10, OutputTokens: 4}})
}
