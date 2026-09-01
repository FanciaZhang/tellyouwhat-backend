package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/capability"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/jobs"
	journalcontracts "github.com/tellyouwhat/backend/internal/journal/contracts"
	journalprovider "github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/recognitionquota"
	"github.com/tellyouwhat/backend/internal/usage"
)

type Principal = attestation.Principal
type RequestProof = attestation.RequestProof

type Authenticator interface {
	Authenticate(context.Context, RequestProof) (Principal, error)
}

type EntitlementChecker interface {
	HasManagedSubscription(context.Context, Principal) (bool, error)
}

type Quota interface {
	Acquire(context.Context, quota.Identity, contracts.Operation, int, string, time.Time) (quota.Releaser, error)
}

type ProviderResponse = providerapi.Response
type StreamEvent = providerapi.StreamEvent
type Provider = providerapi.Client

type ContractValidator interface {
	Validate(contracts.Request) error
}

type Readiness interface {
	Ready(context.Context) error
}

type ReadinessFunc func(context.Context) error

func (function ReadinessFunc) Ready(ctx context.Context) error { return function(ctx) }

type Enrollment interface {
	IssueChallenge(context.Context, string) (attestation.Challenge, error)
	Register(context.Context, attestation.RegistrationRequest) (Principal, error)
}

type EntitlementActivator interface {
	Activate(context.Context, Principal, string) (time.Time, error)
}

type ProductionEntitlementSync interface {
	Sync(context.Context, Principal, string) (time.Time, error)
}

type AppStoreNotificationProcessor interface {
	Process(context.Context, string) (bool, error)
}

type MediaAuthorizer interface {
	Authorize(context.Context, Principal, media.UploadRequest) (media.UploadAuthorization, error)
	Validate(context.Context, Principal, contracts.Request) error
	Consume(context.Context, Principal, contracts.Request, string) error
	Admit(context.Context, Principal, contracts.Request, string) (media.AttemptRecord, bool, error)
}

type JobService interface {
	Enqueue(context.Context, Principal, contracts.Request, string) (jobs.Job, error)
	EnqueueWithID(context.Context, Principal, string, contracts.Request, string) (jobs.Job, error)
	Get(context.Context, Principal, string) (jobs.Job, error)
	Cancel(context.Context, Principal, string) error
}

type JobCapabilityService interface {
	IssueAt(Principal, capability.Binding, time.Time) (capability.Issued, error)
	Validate(string, capability.Binding) (Principal, error)
	Consume(context.Context, string, capability.Binding) (Principal, error)
}

type JobDispatcher interface {
	Dispatch(context.Context, string) error
}

type PrivacyManager interface {
	RecordConsents(context.Context, Principal, []privacy.Consent) (time.Time, error)
	DeletePrincipal(context.Context, Principal) error
}

type ConsentGate interface {
	HasRequiredConsents(context.Context, Principal, []string) (bool, error)
}

type JournalOrganizer interface {
	Organize(context.Context, journalcontracts.OrganizeRequest) (journalprovider.Result, error)
}

type ManagedProduct struct {
	ProductID         string `json:"productID"`
	BillingPeriod     string `json:"billingPeriod"`
	DailyTokenLimit   int    `json:"dailyTokenLimit"`
	MonthlyTokenLimit int    `json:"monthlyTokenLimit"`
	Provider          string `json:"provider"`
	ModelDisclosure   string `json:"modelDisclosure"`
	MediaRetention    string `json:"mediaRetention"`
	JobRetention      string `json:"jobRetention"`
	PrivacyURL        string `json:"privacyURL"`
	TermsURL          string `json:"termsURL"`
	PrivacyChoicesURL string `json:"privacyChoicesURL"`
	SupportURL        string `json:"supportURL"`
}

type Dependencies struct {
	App                        appregistry.App
	Authenticator              Authenticator
	Entitlements               EntitlementChecker
	Quota                      Quota
	QuotaReader                quota.Reader
	FreeRecognitionQuota       Quota
	FreeRecognitionQuotaReader quota.Reader
	RecognitionSessions        recognitionquota.Store
	Provider                   Provider
	IPResolver                 func(*http.Request) string
	Now                        func() time.Time
	Enrollment                 Enrollment
	Activator                  EntitlementActivator
	ProductionEntitlement      ProductionEntitlementSync
	AppStoreNotifications      AppStoreNotificationProcessor
	Media                      MediaAuthorizer
	Jobs                       JobService
	Dispatcher                 JobDispatcher
	Capabilities               JobCapabilityService
	Contracts                  ContractValidator
	Usage                      usage.Recorder
	Readiness                  Readiness
	Privacy                    PrivacyManager
	Consent                    ConsentGate
	RequiredConsentScopes      []string
	JournalOrganizer           JournalOrganizer
	JournalAnalysisVersion     string
	ManagedProduct             ManagedProduct
}

type Server struct {
	app                        appregistry.App
	authenticator              Authenticator
	entitlements               EntitlementChecker
	quota                      Quota
	quotaReader                quota.Reader
	freeRecognitionQuota       Quota
	freeRecognitionQuotaReader quota.Reader
	recognitionSessions        recognitionquota.Store
	provider                   Provider
	ipResolver                 func(*http.Request) string
	now                        func() time.Time
	enrollment                 Enrollment
	activator                  EntitlementActivator
	productionEntitlement      ProductionEntitlementSync
	appStoreNotifications      AppStoreNotificationProcessor
	media                      MediaAuthorizer
	jobs                       JobService
	dispatcher                 JobDispatcher
	capabilities               JobCapabilityService
	contracts                  ContractValidator
	usage                      usage.Recorder
	readiness                  Readiness
	privacy                    PrivacyManager
	consent                    ConsentGate
	requiredConsentScopes      []string
	journalOrganizer           JournalOrganizer
	journalAnalysisVersion     string
	managedProduct             ManagedProduct
	handler                    http.Handler
}

func New(dependencies Dependencies) *Server {
	if dependencies.App.ID == "" {
		allowedOperations := make([]string, 0, len(contracts.OperationValues()))
		for _, operation := range contracts.OperationValues() {
			allowedOperations = append(allowedOperations, string(operation))
		}
		dependencies.App = appregistry.App{
			ID: appregistry.Health, DisplayName: "告你健康", Hosts: []string{"api.health.tellyouwhat.cn"},
			TeamID: "test", BundleID: "cn.tellyouwhat.healthapp", ManagedAIProductID: "health.premium.subscription.monthly",
			AllowedOperations: allowedOperations,
		}
	}
	server := &Server{
		app:                        dependencies.App,
		authenticator:              dependencies.Authenticator,
		entitlements:               dependencies.Entitlements,
		quota:                      dependencies.Quota,
		quotaReader:                dependencies.QuotaReader,
		freeRecognitionQuota:       dependencies.FreeRecognitionQuota,
		freeRecognitionQuotaReader: dependencies.FreeRecognitionQuotaReader,
		recognitionSessions:        dependencies.RecognitionSessions,
		provider:                   dependencies.Provider,
		ipResolver:                 dependencies.IPResolver,
		now:                        dependencies.Now,
		enrollment:                 dependencies.Enrollment,
		activator:                  dependencies.Activator,
		productionEntitlement:      dependencies.ProductionEntitlement,
		appStoreNotifications:      dependencies.AppStoreNotifications,
		media:                      dependencies.Media,
		jobs:                       dependencies.Jobs,
		dispatcher:                 dependencies.Dispatcher,
		capabilities:               dependencies.Capabilities,
		contracts:                  dependencies.Contracts,
		usage:                      dependencies.Usage,
		readiness:                  dependencies.Readiness,
		privacy:                    dependencies.Privacy,
		consent:                    dependencies.Consent,
		requiredConsentScopes:      append([]string(nil), dependencies.RequiredConsentScopes...),
		journalOrganizer:           dependencies.JournalOrganizer,
		journalAnalysisVersion:     dependencies.JournalAnalysisVersion,
		managedProduct:             dependencies.ManagedProduct,
	}
	if server.ipResolver == nil {
		server.ipResolver = remoteIP
	}
	if server.now == nil {
		server.now = time.Now
	}
	if server.app.ID == appregistry.Health {
		server.handler = server.newHTTPRouter()
	} else {
		server.handler = server.newJournalRouter()
	}
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.handler.ServeHTTP(writer, request)
}

func (server *Server) newJournalRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /readyz", server.ready)
	mux.HandleFunc("GET /v1/ai/quota", server.quotaStatus)
	mux.HandleFunc("POST /v1/attest/challenges", server.issueChallenge)
	mux.HandleFunc("POST /v1/attest/keys", server.registerKey)
	mux.HandleFunc("POST /v1/dev/entitlements/activate", server.activateDevelopmentEntitlement)
	mux.HandleFunc("POST /v1/entitlements/transactions", server.syncProductionEntitlement)
	mux.HandleFunc("POST /v1/app-store/notifications", server.processAppStoreNotification)
	if server.app.ID == appregistry.Journal {
		mux.HandleFunc("POST /v1/ai/operations/journal.organize/responses", server.organizeJournal)
	}
	mux.HandleFunc("POST /v1/privacy/consents", server.recordPrivacyConsents)
	mux.HandleFunc("DELETE /v1/privacy/data", server.deletePrivacyData)
	mux.HandleFunc("GET /v1/products/managed-ai", server.managedAIProduct)
	return mux
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) ready(writer http.ResponseWriter, request *http.Request) {
	commonUnavailable := server.authenticator == nil || server.entitlements == nil || server.quota == nil || server.quotaReader == nil ||
		server.enrollment == nil || server.usage == nil || server.readiness == nil || server.privacy == nil || server.consent == nil
	healthUnavailable := server.app.ID == appregistry.Health && (server.provider == nil || server.media == nil || server.jobs == nil || server.dispatcher == nil || server.capabilities == nil || server.contracts == nil)
	journalUnavailable := server.app.ID == appregistry.Journal && (server.journalOrganizer == nil || server.media == nil)
	if commonUnavailable || healthUnavailable || journalUnavailable {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := server.readiness.Ready(ctx); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (server *Server) managedAIProduct(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, server.managedProduct)
}

func (server *Server) recordPrivacyConsents(writer http.ResponseWriter, request *http.Request) {
	if server.privacy == nil {
		writeError(writer, http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	body, principal, ok := server.authenticateRequest(writer, request, 16<<10)
	if !ok {
		return
	}
	var input struct {
		Consents []privacy.Consent `json:"consents"`
	}
	if err := decodeStrictBytes(body, &input); err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	recordedAt, err := server.privacy.RecordConsents(request.Context(), principal, input.Consents)
	if errors.Is(err, privacy.ErrInvalidConsent) {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_consent", "consent scope or document version is invalid", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"recordedAt": recordedAt, "count": len(input.Consents)})
}

func (server *Server) deletePrivacyData(writer http.ResponseWriter, request *http.Request) {
	if server.privacy == nil {
		writeError(writer, http.StatusServiceUnavailable, "privacy_unavailable", "privacy service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	_, principal, ok := server.authenticateRequest(writer, request, 1<<10)
	if !ok {
		return
	}
	if err := server.privacy.DeletePrincipal(request.Context(), principal); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "deletion_unavailable", "data deletion could not be completed", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) quotaStatus(writer http.ResponseWriter, request *http.Request) {
	if server.quotaReader == nil || server.entitlements == nil {
		writeError(writer, http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	_, principal, ok := server.authenticateRequest(writer, request, 1<<10)
	if !ok {
		return
	}
	allowed, err := server.entitlements.HasManagedSubscription(request.Context(), principal)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	if !allowed {
		writeError(writer, http.StatusForbidden, "managed_subscription_required", "managed subscription required", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	snapshot, err := server.quotaReader.Snapshot(request.Context(), transactionID, server.now())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) issueChallenge(writer http.ResponseWriter, request *http.Request) {
	if server.enrollment == nil {
		writeError(writer, http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return
	}
	var input struct {
		KeyID string `json:"keyID"`
	}
	if err := decodeStrictBody(request.Body, 8<<10, &input); err != nil {
		writeMappedError(writer, err, "")
		return
	}
	challenge, err := server.enrollment.IssueChallenge(request.Context(), input.KeyID)
	if err != nil {
		if errors.Is(err, attestation.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		} else {
			writeError(writer, http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		}
		return
	}
	writeJSON(writer, http.StatusCreated, challenge)
}

func (server *Server) registerKey(writer http.ResponseWriter, request *http.Request) {
	if server.enrollment == nil {
		writeError(writer, http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		return
	}
	var input attestation.RegistrationRequest
	if err := decodeStrictBody(request.Body, 512<<10, &input); err != nil {
		writeMappedError(writer, err, "")
		return
	}
	principal, err := server.enrollment.Register(request.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, attestation.ErrUnavailable):
			writeError(writer, http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", "")
		case errors.Is(err, attestation.ErrReplay):
			writeError(writer, http.StatusConflict, "replay_detected", "registration challenge was already used", "")
		case errors.Is(err, attestation.ErrKeyAlreadyRegistered):
			writeError(writer, http.StatusConflict, "key_already_registered", "App Attest key is already registered", "")
		default:
			writeError(writer, http.StatusUnauthorized, "enrollment_denied", "device enrollment denied", "")
		}
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]string{
		"keyID":    principal.KeyID,
		"deviceID": principal.DeviceID,
	})
}

func (server *Server) activateDevelopmentEntitlement(writer http.ResponseWriter, request *http.Request) {
	if server.activator == nil {
		writeError(writer, http.StatusNotFound, "not_found", "route not found", "")
		return
	}
	_, principal, ok := server.authenticateRequest(writer, request, 8<<10)
	if !ok {
		return
	}
	expiresAt, err := server.activator.Activate(
		request.Context(),
		principal,
		request.Header.Get("X-Tellyouwhat-Dev-Activation"),
	)
	if err != nil {
		if errors.Is(err, entitlement.ErrActivationDenied) {
			writeError(writer, http.StatusForbidden, "activation_denied", "development entitlement activation denied", request.Header.Get("X-Tellyouwhat-Request-ID"))
		} else {
			writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "active", "expiresAt": expiresAt})
}

func (server *Server) syncProductionEntitlement(writer http.ResponseWriter, request *http.Request) {
	if server.productionEntitlement == nil {
		writeError(writer, http.StatusNotFound, "not_found", "route not found", "")
		return
	}
	body, principal, ok := server.authenticateRequest(writer, request, 1<<20)
	if !ok {
		return
	}
	var input struct {
		SignedTransaction string `json:"signedTransaction"`
	}
	if err := decodeStrictBytes(body, &input); err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	expiresAt, err := server.productionEntitlement.Sync(
		request.Context(),
		principal,
		input.SignedTransaction,
	)
	if err != nil {
		switch {
		case errors.Is(err, entitlement.ErrProductionSyncDenied):
			writeError(writer, http.StatusForbidden, "transaction_verification_failed", "subscription transaction verification failed", request.Header.Get("X-Tellyouwhat-Request-ID"))
		case errors.Is(err, entitlement.ErrSubscriptionInactive):
			writeError(writer, http.StatusForbidden, "managed_subscription_required", "managed subscription required", request.Header.Get("X-Tellyouwhat-Request-ID"))
		default:
			writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "active", "expiresAt": expiresAt})
}

func (server *Server) processAppStoreNotification(writer http.ResponseWriter, request *http.Request) {
	if server.appStoreNotifications == nil {
		writeError(writer, http.StatusNotFound, "not_found", "route not found", "")
		return
	}
	var input struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := decodeStrictBody(request.Body, 1<<20, &input); err != nil {
		if errors.Is(err, contracts.ErrPayloadTooLarge) {
			writeError(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "notification exceeds the request limit", "")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_notification_envelope", "notification envelope is invalid", "")
		}
		return
	}
	if _, err := server.appStoreNotifications.Process(request.Context(), input.SignedPayload); err != nil {
		if errors.Is(err, entitlement.ErrProductionSyncDenied) {
			writeError(writer, http.StatusBadRequest, "notification_verification_failed", "notification verification failed", "")
		} else {
			writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", "")
		}
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) organizeJournal(writer http.ResponseWriter, request *http.Request) {
	if server.journalOrganizer == nil || server.entitlements == nil || server.quota == nil || server.quotaReader == nil || server.media == nil || server.usage == nil {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", requestID(request))
		return
	}
	body, principal, ok := server.authenticateRequest(writer, request, journalcontracts.MaxBodyBytes)
	if !ok {
		return
	}
	var input journalcontracts.OrganizeRequest
	if err := decodeStrictBytes(body, &input); err != nil || input.Validate() != nil {
		writeError(writer, http.StatusUnprocessableEntity, "contract_violation", "request violates the journal organization contract", requestID(request))
		return
	}
	if input.RequestID != requestID(request) || !server.app.AllowsOperation("journal.organize") {
		writeError(writer, http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID(request))
		return
	}
	allowed, err := server.entitlements.HasManagedSubscription(request.Context(), principal)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", input.RequestID)
		return
	}
	if !allowed {
		writeError(writer, http.StatusForbidden, "managed_subscription_required", "managed subscription required", input.RequestID)
		return
	}
	if !server.requireConsent(writer, request, principal, input.RequestID) {
		return
	}
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	estimatedTokens := journalReservationTokens(input)
	lease, err := server.quota.Acquire(request.Context(), quota.Identity{
		DeviceID: principal.DeviceID, TransactionID: transactionID, IP: server.ipResolver(request),
	}, contracts.Operation("journal.organize"), estimatedTokens, "", server.now())
	if err != nil {
		if errors.Is(err, quota.ErrExceeded) {
			code, message := journalQuotaExceededResponse(err)
			writeError(writer, http.StatusTooManyRequests, code, message, input.RequestID)
		} else {
			writeError(writer, http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", input.RequestID)
		}
		return
	}
	artifact := contracts.Request{
		RequestID: input.RequestID,
		Operation: contracts.Operation("journal.organize"),
	}
	if err := server.media.Consume(request.Context(), principal, artifact, contracts.BodySHA256(body)); err != nil {
		lease.Release(0)
		server.writeAdmissionError(writer, err, input.RequestID)
		return
	}
	result, err := server.journalOrganizer.Organize(request.Context(), input)
	if err != nil {
		lease.Release(0)
		writeError(writer, http.StatusBadGateway, "upstream_error", "managed AI provider failed", input.RequestID)
		return
	}
	actualTokens := result.InputTokens + result.OutputTokens
	if err := server.usage.Record(request.Context(), usage.Record{
		RequestID: input.RequestID, KeyID: principal.KeyID, DeviceID: principal.DeviceID,
		TransactionID: transactionID, Operation: contracts.Operation("journal.organize"),
		InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, OccurredAt: server.now(),
	}); err != nil {
		actualTokens = estimatedTokens
	}
	lease.Release(actualTokens)
	snapshot, snapshotErr := server.quotaReader.Snapshot(request.Context(), transactionID, server.now())
	response := journalcontracts.OrganizeResponse{
		RequestID: input.RequestID, ContentHash: input.ContentHash,
		AnalysisVersion:             server.journalAnalysisVersion,
		Tags:                        result.Value.Tags,
		ExistingBookRecommendations: result.Value.ExistingBookRecommendations,
		NewBookSuggestions:          result.Value.NewBookSuggestions,
		Quota: journalcontracts.Quota{
			DailyTokensRemaining:   max(0, snapshot.DailyLimit-snapshot.DailyUsed),
			MonthlyTokensRemaining: max(0, snapshot.MonthlyLimit-snapshot.MonthlyUsed),
			Available:              snapshotErr == nil,
		},
	}
	bookIDs := make(map[string]bool, len(input.Books))
	for _, book := range input.Books {
		bookIDs[book.ID] = true
	}
	if err := response.Validate(bookIDs); err != nil {
		writeError(writer, http.StatusBadGateway, "invalid_model_result", "managed AI returned an invalid result", input.RequestID)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func journalReservationTokens(input journalcontracts.OrganizeRequest) int {
	value := len(input.Title) + len(input.Body) + 8_192
	for _, tag := range input.ExistingTags {
		value += len(tag)
	}
	for _, tag := range input.RejectedTagNames {
		value += len(tag)
	}
	for _, book := range input.Books {
		value += len(book.Name) + len(book.Description) + 64
	}
	return value
}

func (server *Server) requireConsent(
	writer http.ResponseWriter,
	request *http.Request,
	principal Principal,
	requestID string,
) bool {
	if len(server.requiredConsentScopes) == 0 {
		return true
	}
	if server.consent == nil {
		writeError(writer, http.StatusServiceUnavailable, "consent_unavailable", "privacy consent service unavailable", requestID)
		return false
	}
	granted, err := server.consent.HasRequiredConsents(request.Context(), principal, server.requiredConsentScopes)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "consent_unavailable", "privacy consent service unavailable", requestID)
		return false
	}
	if !granted {
		writeError(writer, http.StatusForbidden, "consent_required", "required privacy consent has not been granted", requestID)
		return false
	}
	return true
}

func journalQuotaExceededResponse(err error) (string, string) {
	scope, _ := quota.ExceededScope(err)
	switch scope {
	case quota.LimitDailyTokens:
		return "daily_quota_exceeded", "daily managed AI safety limit reached"
	case quota.LimitMonthlyTokens:
		return "monthly_quota_exceeded", "monthly managed AI safety limit reached"
	case quota.LimitConcurrentDevice:
		return "concurrency_limit_exceeded", "managed AI requests are already in progress"
	case quota.LimitRequestsPerMinuteIP,
		quota.LimitRequestsPerMinuteDevice,
		quota.LimitRequestsPerMinuteOperation:
		return "rate_limit_exceeded", "managed AI requests are arriving too quickly"
	default:
		return "quota_exceeded", "managed AI quota exceeded"
	}
}

func (server *Server) writeAdmissionError(writer http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, media.ErrIdempotencyReplay), errors.Is(err, media.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency_conflict", "requestID was already used", requestID)
	case errors.Is(err, media.ErrNotAuthorized):
		writeError(writer, http.StatusUnprocessableEntity, "contract_violation", "request violates the request authorization contract", requestID)
	default:
		writeError(writer, http.StatusServiceUnavailable, "request_commit_unavailable", "request authorization could not be committed", requestID)
	}
}

func (server *Server) authenticateRequest(
	writer http.ResponseWriter,
	request *http.Request,
	maxBodyBytes int64,
) ([]byte, Principal, bool) {
	if server.authenticator == nil {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "authentication service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return nil, Principal{}, false
	}
	body, err := readBody(request.Body, maxBodyBytes)
	if err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return nil, Principal{}, false
	}
	proof := RequestProof{
		Method:     request.Method,
		Path:       request.URL.EscapedPath(),
		RequestID:  request.Header.Get("X-Tellyouwhat-Request-ID"),
		KeyID:      request.Header.Get("X-Tellyouwhat-Key-ID"),
		Assertion:  request.Header.Get("X-Tellyouwhat-Assertion"),
		Nonce:      request.Header.Get("X-Tellyouwhat-Nonce"),
		Timestamp:  request.Header.Get("X-Tellyouwhat-Timestamp"),
		BodySHA256: contracts.BodySHA256(body),
	}
	principal, err := server.authenticator.Authenticate(request.Context(), proof)
	if err != nil {
		writeAuthenticationError(writer, err, proof.RequestID)
		return nil, Principal{}, false
	}
	if principal.AppID != string(server.app.ID) {
		writeError(writer, http.StatusUnauthorized, "authentication_failed", "request authentication failed", proof.RequestID)
		return nil, Principal{}, false
	}
	return body, principal, true
}

func requestID(request *http.Request) string {
	return request.Header.Get("X-Tellyouwhat-Request-ID")
}

func writeAuthenticationError(writer http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, attestation.ErrReplay):
		writeError(writer, http.StatusConflict, "replay_detected", "request nonce or assertion was already used", requestID)
	case errors.Is(err, attestation.ErrUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "attestation_unavailable", "attestation service unavailable", requestID)
	default:
		writeError(writer, http.StatusUnauthorized, "authentication_failed", "request authentication failed", requestID)
	}
}

func remoteIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return request.RemoteAddr
}

func readBody(reader io.ReadCloser, maxBytes int64) ([]byte, error) {
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body", contracts.ErrContractViolation)
	}
	if int64(len(body)) > maxBytes {
		return nil, contracts.ErrPayloadTooLarge
	}
	return body, nil
}

func decodeStrictBody(reader io.ReadCloser, maxBytes int64, destination any) error {
	body, err := readBody(reader, maxBytes)
	if err != nil {
		return err
	}
	return decodeStrictBytes(body, destination)
}

func decodeStrictBytes(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: invalid JSON", contracts.ErrContractViolation)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", contracts.ErrContractViolation)
	}
	return nil
}

func writeMappedError(writer http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, contracts.ErrPayloadTooLarge):
		writeError(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "request exceeds the operation limit", requestID)
	case errors.Is(err, contracts.ErrUpgradeRequired):
		writeError(writer, http.StatusUpgradeRequired, "upgrade_required", "this app version is no longer supported", requestID)
	default:
		writeError(writer, http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", requestID)
	}
}

func writeError(writer http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]string{
			"code": code, "message": message, "requestID": requestID,
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
