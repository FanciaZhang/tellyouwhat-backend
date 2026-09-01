package privacy

import (
	"context"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

const (
	AdultScope           = "adult"
	PrivacyTermsScope    = "privacy_and_terms"
	LifetimeBYOKScope    = "lifetime_byok"
	ManagedAIScope       = "managed_subscription"
	FreeRecognitionScope = "free_managed_recognition"
	SensitiveHealthScope = "sensitive_health_ai"

	GeneralDocumentVersion = "2026-08-24"
	AIDocumentVersion      = "2026-08-24"
)

var ErrInvalidConsent = errors.New("invalid privacy consent")
var ErrConsentUnavailable = errors.New("privacy consent status unavailable")

type Consent struct {
	Scope           string `json:"scope"`
	DocumentVersion string `json:"documentVersion"`
	Granted         bool   `json:"granted"`
}

type Record struct {
	KeyID           string
	DeviceID        string
	Scope           string
	DocumentVersion string
	Granted         bool
	RecordedAt      time.Time
}

type Repository interface {
	RecordConsents(context.Context, []Record) error
	PlanDeletion(context.Context, attestation.Principal) (DeletionPlan, error)
	DeletePrincipal(context.Context, attestation.Principal) error
}

type ConsentReader interface {
	HasGrantedConsents(context.Context, string, []Consent) (bool, error)
}

type DeletionPlan struct {
	Principals     []attestation.Principal
	MediaObjectIDs []string
}

type ObjectCleaner interface {
	DeleteObject(context.Context, string) error
}

type CacheCleaner interface {
	DeletePrincipal(context.Context, attestation.Principal) error
}

type Service struct {
	repository Repository
	objects    ObjectCleaner
	cache      CacheCleaner
	now        func() time.Time
}

func NewService(repository Repository, objects ObjectCleaner, cache CacheCleaner, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, objects: objects, cache: cache, now: now}
}

func (service *Service) RecordConsents(ctx context.Context, principal attestation.Principal, values []Consent) (time.Time, error) {
	if service == nil || service.repository == nil || principal.KeyID == "" || principal.DeviceID == "" || len(values) == 0 || len(values) > 6 {
		return time.Time{}, ErrInvalidConsent
	}
	recordedAt := service.now().UTC().Truncate(time.Microsecond)
	records := make([]Record, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validConsent(value) {
			return time.Time{}, ErrInvalidConsent
		}
		key := value.Scope + "\x00" + value.DocumentVersion
		if _, exists := seen[key]; exists {
			return time.Time{}, ErrInvalidConsent
		}
		seen[key] = struct{}{}
		records = append(records, Record{
			KeyID: principal.KeyID, DeviceID: principal.DeviceID, Scope: value.Scope,
			DocumentVersion: value.DocumentVersion, Granted: value.Granted, RecordedAt: recordedAt,
		})
	}
	if err := service.repository.RecordConsents(ctx, records); err != nil {
		return time.Time{}, err
	}
	return recordedAt, nil
}

func (service *Service) HasRequiredConsents(
	ctx context.Context,
	principal attestation.Principal,
	scopes []string,
) (bool, error) {
	if service == nil || principal.KeyID == "" || len(scopes) == 0 {
		return false, ErrConsentUnavailable
	}
	reader, ok := service.repository.(ConsentReader)
	if !ok {
		return false, ErrConsentUnavailable
	}
	requirements := make([]Consent, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		requirement := Consent{Scope: scope, Granted: true}
		switch scope {
		case AdultScope, PrivacyTermsScope:
			requirement.DocumentVersion = GeneralDocumentVersion
		case LifetimeBYOKScope, ManagedAIScope, FreeRecognitionScope, SensitiveHealthScope:
			requirement.DocumentVersion = AIDocumentVersion
		default:
			return false, ErrInvalidConsent
		}
		requirements = append(requirements, requirement)
	}
	return reader.HasGrantedConsents(ctx, principal.KeyID, requirements)
}

func (service *Service) DeletePrincipal(ctx context.Context, principal attestation.Principal) error {
	if service == nil || service.repository == nil || service.objects == nil || principal.KeyID == "" || principal.DeviceID == "" {
		return errors.New("privacy deletion service unavailable")
	}
	plan, err := service.repository.PlanDeletion(ctx, principal)
	if err != nil {
		return err
	}
	for _, objectID := range plan.MediaObjectIDs {
		if err := service.objects.DeleteObject(ctx, objectID); err != nil {
			return err
		}
	}
	if service.cache != nil {
		for _, plannedPrincipal := range plan.Principals {
			if err := service.cache.DeletePrincipal(ctx, plannedPrincipal); err != nil {
				return err
			}
		}
	}
	return service.repository.DeletePrincipal(ctx, principal)
}

func validConsent(value Consent) bool {
	switch value.Scope {
	case AdultScope, PrivacyTermsScope:
		return value.DocumentVersion == GeneralDocumentVersion
	case LifetimeBYOKScope, ManagedAIScope, FreeRecognitionScope, SensitiveHealthScope:
		return value.DocumentVersion == AIDocumentVersion
	default:
		return false
	}
}
