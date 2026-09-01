package gateway

import (
	"context"
	"net"
	"net/http"
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
		server.handler = server.newJournalHTTPRouter()
	}
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.handler.ServeHTTP(writer, request)
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

func remoteIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return request.RemoteAddr
}
