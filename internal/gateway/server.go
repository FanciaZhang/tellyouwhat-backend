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
	"strings"
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
	"github.com/tellyouwhat/backend/internal/usage"
)

var (
	ErrAuthentication = errors.New("authentication failed")
	ErrNoEntitlement  = errors.New("managed subscription required")
	ErrQuotaExceeded  = errors.New("quota exceeded")
	ErrUpstream       = errors.New("upstream provider error")
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
	App                    appregistry.App
	Authenticator          Authenticator
	Entitlements           EntitlementChecker
	Quota                  Quota
	QuotaReader            quota.Reader
	Provider               Provider
	IPResolver             func(*http.Request) string
	Now                    func() time.Time
	Enrollment             Enrollment
	Activator              EntitlementActivator
	ProductionEntitlement  ProductionEntitlementSync
	AppStoreNotifications  AppStoreNotificationProcessor
	Media                  MediaAuthorizer
	Jobs                   JobService
	Dispatcher             JobDispatcher
	Capabilities           JobCapabilityService
	Contracts              ContractValidator
	Usage                  usage.Recorder
	Readiness              Readiness
	Privacy                PrivacyManager
	Consent                ConsentGate
	RequiredConsentScopes  []string
	JournalOrganizer       JournalOrganizer
	JournalAnalysisVersion string
	ManagedProduct         ManagedProduct
}

type Server struct {
	app                    appregistry.App
	authenticator          Authenticator
	entitlements           EntitlementChecker
	quota                  Quota
	quotaReader            quota.Reader
	provider               Provider
	ipResolver             func(*http.Request) string
	now                    func() time.Time
	enrollment             Enrollment
	activator              EntitlementActivator
	productionEntitlement  ProductionEntitlementSync
	appStoreNotifications  AppStoreNotificationProcessor
	media                  MediaAuthorizer
	jobs                   JobService
	dispatcher             JobDispatcher
	capabilities           JobCapabilityService
	contracts              ContractValidator
	usage                  usage.Recorder
	readiness              Readiness
	privacy                PrivacyManager
	consent                ConsentGate
	requiredConsentScopes  []string
	journalOrganizer       JournalOrganizer
	journalAnalysisVersion string
	managedProduct         ManagedProduct
	mux                    *http.ServeMux
}

func New(dependencies Dependencies) *Server {
	if dependencies.App.ID == "" {
		dependencies.App = appregistry.App{
			ID: appregistry.Health, DisplayName: "告你健康", Hosts: []string{"api.health.tellyouwhat.cn"},
			TeamID: "test", BundleID: "cn.tellyouwhat.healthapp", ManagedAIProductID: "health.ai.subscription.monthly",
			AllowedOperationPrefix: "health.",
		}
	}
	server := &Server{
		app:                    dependencies.App,
		authenticator:          dependencies.Authenticator,
		entitlements:           dependencies.Entitlements,
		quota:                  dependencies.Quota,
		quotaReader:            dependencies.QuotaReader,
		provider:               dependencies.Provider,
		ipResolver:             dependencies.IPResolver,
		now:                    dependencies.Now,
		enrollment:             dependencies.Enrollment,
		activator:              dependencies.Activator,
		productionEntitlement:  dependencies.ProductionEntitlement,
		appStoreNotifications:  dependencies.AppStoreNotifications,
		media:                  dependencies.Media,
		jobs:                   dependencies.Jobs,
		dispatcher:             dependencies.Dispatcher,
		capabilities:           dependencies.Capabilities,
		contracts:              dependencies.Contracts,
		usage:                  dependencies.Usage,
		readiness:              dependencies.Readiness,
		privacy:                dependencies.Privacy,
		consent:                dependencies.Consent,
		requiredConsentScopes:  append([]string(nil), dependencies.RequiredConsentScopes...),
		journalOrganizer:       dependencies.JournalOrganizer,
		journalAnalysisVersion: dependencies.JournalAnalysisVersion,
		managedProduct:         dependencies.ManagedProduct,
		mux:                    http.NewServeMux(),
	}
	if server.ipResolver == nil {
		server.ipResolver = remoteIP
	}
	if server.now == nil {
		server.now = time.Now
	}
	server.routes()
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", server.health)
	server.mux.HandleFunc("GET /readyz", server.ready)
	server.mux.HandleFunc("GET /v1/ai/quota", server.quotaStatus)
	server.mux.HandleFunc("POST /v1/attest/challenges", server.issueChallenge)
	server.mux.HandleFunc("POST /v1/attest/keys", server.registerKey)
	server.mux.HandleFunc("POST /v1/dev/entitlements/activate", server.activateDevelopmentEntitlement)
	server.mux.HandleFunc("POST /v1/entitlements/transactions", server.syncProductionEntitlement)
	server.mux.HandleFunc("POST /v1/app-store/notifications", server.processAppStoreNotification)
	if server.app.ID == appregistry.Health {
		server.mux.HandleFunc("POST /v1/media/upload-authorizations", server.authorizeMediaUpload)
		server.mux.HandleFunc("POST /v1/ai/operations/{operation}/responses", server.complete)
		server.mux.HandleFunc("POST /v1/ai/operations/{operation}/streams", server.stream)
		server.mux.HandleFunc("POST /v1/ai/operations/{operation}/job-capabilities", server.issueJobCapability)
		server.mux.HandleFunc("POST /v1/ai/jobs", server.enqueueJob)
		server.mux.HandleFunc("GET /v1/ai/jobs/{id}", server.getJob)
		server.mux.HandleFunc("DELETE /v1/ai/jobs/{id}", server.cancelJob)
	}
	if server.app.ID == appregistry.Journal {
		server.mux.HandleFunc("POST /v1/ai/operations/journal.organize/responses", server.organizeJournal)
	}
	server.mux.HandleFunc("POST /v1/privacy/consents", server.recordPrivacyConsents)
	server.mux.HandleFunc("DELETE /v1/privacy/data", server.deletePrivacyData)
	server.mux.HandleFunc("GET /v1/products/managed-ai", server.managedAIProduct)
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
		case errors.Is(err, attestation.ErrReplay), errors.Is(err, attestation.ErrKeyAlreadyRegistered):
			writeError(writer, http.StatusConflict, "replay_detected", "registration challenge was already used", "")
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

func (server *Server) authorizeMediaUpload(writer http.ResponseWriter, request *http.Request) {
	if server.media == nil || server.entitlements == nil {
		writeError(writer, http.StatusServiceUnavailable, "media_unavailable", "media service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	body, principal, ok := server.authenticateRequest(writer, request, 64<<10)
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
	var input media.UploadRequest
	if err := decodeStrictBytes(body, &input); err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	authorization, err := server.media.Authorize(request.Context(), principal, input)
	if err != nil {
		switch {
		case errors.Is(err, contracts.ErrContractViolation), errors.Is(err, contracts.ErrPayloadTooLarge):
			writeMappedError(writer, err, input.RequestID)
		case errors.Is(err, media.ErrAuthorizationConflict):
			writeError(writer, http.StatusConflict, "media_authorization_conflict", "media authorization already exists with different metadata", input.RequestID)
		default:
			writeError(writer, http.StatusServiceUnavailable, "media_unavailable", "media service unavailable", input.RequestID)
		}
		return
	}
	writeJSON(writer, http.StatusCreated, authorization)
}

func (server *Server) enqueueJob(writer http.ResponseWriter, request *http.Request) {
	if server.jobs == nil || server.dispatcher == nil || server.capabilities == nil || server.entitlements == nil || server.contracts == nil || server.media == nil {
		writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	body, err := readBody(request.Body, contracts.DefaultBodyLimit)
	if err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	artifact, err := contracts.DecodeAndValidate(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	if err := server.contracts.Validate(artifact); err != nil {
		writeMappedError(writer, err, artifact.RequestID)
		return
	}
	jobID := request.Header.Get("X-Tellyouwhat-Job-ID")
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "JobCapability ")
	mediaDigest, err := contracts.MediaDigest(artifact.Media)
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", artifact.RequestID)
		return
	}
	binding := capability.Binding{
		JobID:       jobID,
		RequestID:   artifact.RequestID,
		Operation:   artifact.Operation,
		BodyDigest:  contracts.BodySHA256(body),
		MediaDigest: mediaDigest,
	}
	principal, err := server.capabilities.Validate(token, binding)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "job_capability_invalid", "job capability is invalid or expired", artifact.RequestID)
		return
	}
	if principal.AppID != "" && principal.AppID != string(server.app.ID) {
		writeError(writer, http.StatusUnauthorized, "job_capability_invalid", "job capability is invalid or expired", artifact.RequestID)
		return
	}
	allowed, err := server.entitlements.HasManagedSubscription(request.Context(), principal)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", artifact.RequestID)
		return
	}
	if !allowed {
		writeError(writer, http.StatusForbidden, "managed_subscription_required", "managed subscription required", artifact.RequestID)
		return
	}
	if !server.requireConsent(writer, request, principal, artifact.RequestID) {
		return
	}
	job, err := server.jobs.EnqueueWithID(request.Context(), principal, jobID, artifact, contracts.BodySHA256(body))
	if err != nil {
		if errors.Is(err, jobs.ErrIdempotencyConflict) {
			writeError(writer, http.StatusConflict, "idempotency_conflict", "requestID was already used with different content", artifact.RequestID)
		} else {
			writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		}
		return
	}
	// Consume only after the job and its outbox row are durable. Replays and
	// transient use-store failures remain safe because the signed capability is
	// bound to the same job ID and request digest, and EnqueueWithID is idempotent.
	_, _ = server.capabilities.Consume(request.Context(), token, binding)
	if err := server.dispatcher.Dispatch(request.Context(), job.ID); err != nil {
		// The transactional outbox owns eventual dispatch; this call is only an
		// eager wake-up and must not turn a durable accepted job into an error.
	}
	writeJSON(writer, http.StatusAccepted, publicJob(job))
}

func (server *Server) issueJobCapability(writer http.ResponseWriter, request *http.Request) {
	if server.capabilities == nil || server.jobs == nil || server.dispatcher == nil {
		writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	artifact, body, principal, ok := server.validateAIRequest(writer, request)
	if !ok {
		return
	}
	bodyDigest := contracts.BodySHA256(body)
	lease, ok := server.acquireQuota(writer, request, principal, artifact, capabilityQuotaReservationID(principal, artifact.RequestID, bodyDigest))
	if !ok {
		return
	}
	attempt, _, err := server.media.Admit(request.Context(), principal, artifact, bodyDigest)
	if err != nil {
		// A stable capability reservation can be shared by concurrent exact
		// retries. Keep it conservatively even when this caller's admission
		// attempt fails; another no-op lease may already have admitted the same
		// artifact. The reservation expires with the quota window.
		lease.Release(contracts.ReservationTokens(artifact))
		server.writeAdmissionError(writer, err, artifact.RequestID)
		return
	}
	// The quota layer owns reservation idempotency. Any successful admission
	// retains exactly one conservative reservation; a replay lease is a no-op.
	// Basing this decision on the media replay bit is unsafe because concurrent
	// callers can acquire and admit in opposite orders.
	lease.Release(contracts.ReservationTokens(artifact))
	mediaDigest, err := contracts.MediaDigest(artifact.Media)
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "contract_violation", "request violates the business contract", artifact.RequestID)
		return
	}
	issued, err := server.capabilities.IssueAt(principal, capability.Binding{
		RequestID:   artifact.RequestID,
		Operation:   artifact.Operation,
		BodyDigest:  bodyDigest,
		MediaDigest: mediaDigest,
	}, attempt.CreatedAt)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", artifact.RequestID)
		return
	}
	writeJSON(writer, http.StatusCreated, issued)
}

func (server *Server) getJob(writer http.ResponseWriter, request *http.Request) {
	if server.jobs == nil {
		writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	_, principal, ok := server.authenticateRequest(writer, request, 0)
	if !ok {
		return
	}
	job, err := server.jobs.Get(request.Context(), principal, request.PathValue("id"))
	if err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "job_not_found", "job not found", request.Header.Get("X-Tellyouwhat-Request-ID"))
		} else {
			writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		}
		return
	}
	writeJSON(writer, http.StatusOK, publicJob(job))
}

func (server *Server) cancelJob(writer http.ResponseWriter, request *http.Request) {
	if server.jobs == nil {
		writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		return
	}
	_, principal, ok := server.authenticateRequest(writer, request, 0)
	if !ok {
		return
	}
	if err := server.jobs.Cancel(request.Context(), principal, request.PathValue("id")); err != nil {
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			writeError(writer, http.StatusNotFound, "job_not_found", "job not found", request.Header.Get("X-Tellyouwhat-Request-ID"))
		case errors.Is(err, jobs.ErrJobNotClaimable):
			writeError(writer, http.StatusConflict, "job_not_cancellable", "job is already complete", request.Header.Get("X-Tellyouwhat-Request-ID"))
		default:
			writeError(writer, http.StatusServiceUnavailable, "jobs_unavailable", "job service unavailable", request.Header.Get("X-Tellyouwhat-Request-ID"))
		}
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func publicJob(job jobs.Job) map[string]any {
	value := map[string]any{
		"jobID":     job.ID,
		"requestID": job.RequestID,
		"status":    job.Status,
		"createdAt": job.CreatedAt,
		"updatedAt": job.UpdatedAt,
	}
	if job.Status == jobs.StatusSucceeded {
		value["content"] = job.Result
		value["usage"] = map[string]int{"inputTokens": job.InputTokens, "outputTokens": job.OutputTokens}
	}
	if job.Status == jobs.StatusFailed {
		value["errorCategory"] = job.FailureCategory
	}
	return value
}

func (server *Server) complete(writer http.ResponseWriter, request *http.Request) {
	artifact, _, principal, lease, ok := server.authorizeAIRequest(writer, request)
	if !ok {
		return
	}
	response, err := server.provider.Complete(request.Context(), artifact)
	if err != nil {
		lease.Release(contracts.ReservationTokens(artifact))
		writeError(writer, http.StatusBadGateway, "upstream_error", "managed AI provider failed", artifact.RequestID)
		return
	}
	actualTokens := response.InputTokens + response.OutputTokens
	if err := server.recordUsage(request.Context(), principal, artifact, response.InputTokens, response.OutputTokens); err != nil {
		// Ark already returned a valid result. Deliver it so the client does not
		// repeat a billable request; retaining the conservative reservation is
		// the safe accounting fallback when the durable ledger is unavailable.
		actualTokens = contracts.ReservationTokens(artifact)
	}
	cleanupManagedMedia(request.Context(), server.provider, artifact.Media)
	lease.Release(actualTokens)
	writeJSON(writer, http.StatusOK, map[string]any{
		"requestID": artifact.RequestID,
		"content":   response.Content,
		"usage": map[string]int{
			"inputTokens":  response.InputTokens,
			"outputTokens": response.OutputTokens,
		},
	})
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
			code, message := quotaExceededResponse(err)
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
	snapshot, err := server.quotaReader.Snapshot(request.Context(), transactionID, server.now())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", input.RequestID)
		return
	}
	response := journalcontracts.OrganizeResponse{
		RequestID: input.RequestID, ContentHash: input.ContentHash,
		AnalysisVersion:             server.journalAnalysisVersion,
		Tags:                        result.Value.Tags,
		ExistingBookRecommendations: result.Value.ExistingBookRecommendations,
		NewBookSuggestions:          result.Value.NewBookSuggestions,
		Quota: journalcontracts.Quota{
			DailyTokensRemaining:   max(0, snapshot.DailyLimit-snapshot.DailyUsed),
			MonthlyTokensRemaining: max(0, snapshot.MonthlyLimit-snapshot.MonthlyUsed),
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

func (server *Server) stream(writer http.ResponseWriter, request *http.Request) {
	artifact, _, principal, lease, ok := server.authorizeAIRequest(writer, request)
	if !ok {
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		lease.Release(0)
		writeError(writer, http.StatusServiceUnavailable, "stream_unavailable", "streaming is unavailable", artifact.RequestID)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	actualTokens := contracts.ReservationTokens(artifact)

	err := server.provider.Stream(request.Context(), artifact, func(event StreamEvent) error {
		switch {
		case event.Completed != nil:
			if err := server.recordUsage(request.Context(), principal, artifact, event.Completed.InputTokens, event.Completed.OutputTokens); err == nil {
				actualTokens = event.Completed.InputTokens + event.Completed.OutputTokens
			}
			if err := writeSSE(writer, "completed", map[string]any{
				"requestID": artifact.RequestID,
				"content":   event.Completed.Content,
				"usage": map[string]int{
					"inputTokens":  event.Completed.InputTokens,
					"outputTokens": event.Completed.OutputTokens,
				},
			}); err != nil {
				return err
			}
		case event.Delta != "":
			if err := writeSSE(writer, "delta", map[string]string{"delta": event.Delta}); err != nil {
				return err
			}
		default:
			return nil
		}
		flusher.Flush()
		return nil
	})
	if err == nil {
		cleanupManagedMedia(request.Context(), server.provider, artifact.Media)
	}
	lease.Release(actualTokens)
	if err != nil {
		_ = writeSSE(writer, "error", map[string]string{
			"code":      "upstream_error",
			"requestID": artifact.RequestID,
		})
		flusher.Flush()
	}
}

func cleanupManagedMedia(ctx context.Context, provider Provider, values []contracts.Media) {
	cleaner, ok := provider.(providerapi.ManagedMediaCleaner)
	if !ok || len(values) == 0 {
		return
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cleaner.CleanupManagedMedia(cleanupContext, values)
}

func (server *Server) authorizeAIRequest(
	writer http.ResponseWriter,
	request *http.Request,
) (contracts.Request, []byte, Principal, quota.Releaser, bool) {
	artifact, body, principal, ok := server.validateAIRequest(writer, request)
	if !ok {
		return contracts.Request{}, nil, Principal{}, nil, false
	}
	lease, ok := server.acquireQuota(writer, request, principal, artifact, "")
	if !ok {
		return contracts.Request{}, nil, Principal{}, nil, false
	}
	if err := server.media.Consume(request.Context(), principal, artifact, contracts.BodySHA256(body)); err != nil {
		lease.Release(0)
		server.writeAdmissionError(writer, err, artifact.RequestID)
		return contracts.Request{}, nil, Principal{}, nil, false
	}
	return artifact, body, principal, lease, true
}

func (server *Server) validateAIRequest(
	writer http.ResponseWriter,
	request *http.Request,
) (contracts.Request, []byte, Principal, bool) {
	if server.authenticator == nil || server.entitlements == nil || server.quota == nil || server.provider == nil || server.contracts == nil || server.media == nil || server.usage == nil {
		writeError(writer, http.StatusServiceUnavailable, "not_ready", "service dependencies are unavailable", "")
		return contracts.Request{}, nil, Principal{}, false
	}
	body, err := readBody(request.Body, contracts.DefaultBodyLimit)
	if err != nil {
		writeMappedError(writer, err, request.Header.Get("X-Tellyouwhat-Request-ID"))
		return contracts.Request{}, nil, Principal{}, false
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
		return contracts.Request{}, nil, Principal{}, false
	}
	if principal.AppID != "" && principal.AppID != string(server.app.ID) {
		writeError(writer, http.StatusUnauthorized, "authentication_failed", "request authentication failed", proof.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	artifact, err := contracts.DecodeAndValidate(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writeMappedError(writer, err, proof.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	if err := server.contracts.Validate(artifact); err != nil {
		writeMappedError(writer, err, proof.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	if proof.RequestID != artifact.RequestID {
		writeError(writer, http.StatusUnauthorized, "authentication_failed", "request authentication failed", proof.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	pathOperation := request.PathValue("operation")
	if pathOperation == "" || pathOperation != string(artifact.Operation) || !server.app.AllowsOperation(pathOperation) {
		writeError(writer, http.StatusUnprocessableEntity, "operation_mismatch", "request operation does not match the endpoint", artifact.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	allowed, err := server.entitlements.HasManagedSubscription(request.Context(), principal)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "entitlement_unavailable", "entitlement service unavailable", artifact.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	if !allowed {
		writeError(writer, http.StatusForbidden, "managed_subscription_required", "managed subscription required", artifact.RequestID)
		return contracts.Request{}, nil, Principal{}, false
	}
	if !server.requireConsent(writer, request, principal, artifact.RequestID) {
		return contracts.Request{}, nil, Principal{}, false
	}
	return artifact, body, principal, true
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

func (server *Server) acquireQuota(
	writer http.ResponseWriter,
	request *http.Request,
	principal Principal,
	artifact contracts.Request,
	reservationID string,
) (quota.Releaser, bool) {
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	lease, err := server.quota.Acquire(request.Context(), quota.Identity{
		DeviceID:      principal.DeviceID,
		TransactionID: transactionID,
		IP:            server.ipResolver(request),
	}, artifact.Operation, contracts.ReservationTokens(artifact), reservationID, server.now())
	if err != nil {
		if errors.Is(err, quota.ErrExceeded) || errors.Is(err, ErrQuotaExceeded) {
			code, message := quotaExceededResponse(err)
			writeError(writer, http.StatusTooManyRequests, code, message, artifact.RequestID)
		} else {
			writeError(writer, http.StatusServiceUnavailable, "quota_unavailable", "quota service unavailable", artifact.RequestID)
		}
		return nil, false
	}
	return lease, true
}

func quotaExceededResponse(err error) (string, string) {
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

func capabilityQuotaReservationID(principal Principal, requestID, bodyDigest string) string {
	return contracts.BodySHA256([]byte("job-capability\n" + principal.KeyID + "\n" + requestID + "\n" + bodyDigest))
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

func (server *Server) recordUsage(
	ctx context.Context,
	principal Principal,
	request contracts.Request,
	inputTokens,
	outputTokens int,
) error {
	transactionID := principal.TransactionID
	if transactionID == "" {
		transactionID = principal.KeyID
	}
	return server.usage.Record(ctx, usage.Record{
		RequestID:     request.RequestID,
		KeyID:         principal.KeyID,
		DeviceID:      principal.DeviceID,
		TransactionID: transactionID,
		Operation:     request.Operation,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		OccurredAt:    server.now(),
	})
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
	if principal.AppID != "" && principal.AppID != string(server.app.ID) {
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
			"code":      code,
			"message":   message,
			"requestID": requestID,
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeSSE(writer io.Writer, event string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if strings.ContainsAny(event, "\r\n") {
		return errors.New("invalid SSE event")
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded)
	return err
}
