package entitlement

import (
	"context"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/purchase"
)

var ErrActivationDenied = errors.New("development entitlement activation denied")

type Record struct {
	Payment            purchase.Evidence
	KeyID              string
	TransactionID      string
	ExpiresAt          time.Time
	StartedAt          time.Time
	Environment        string
	OfferTransactionID string
	OfferIdentifier    string
	ProductID          string
	OfferType          int32
	OfferSignedAt      time.Time
}

type Store interface {
	Upsert(context.Context, Record) error
	Get(context.Context, string) (Record, bool, error)
}

type DevelopmentService struct {
	store       Store
	appID       string
	duration    time.Duration
	now         func() time.Time
	environment string
}

type Checker struct {
	store Store
	now   func() time.Time
}

func NewChecker(store Store, now func() time.Time) *Checker {
	if now == nil {
		now = time.Now
	}
	return &Checker{store: store, now: now}
}

func (checker *Checker) HasManagedSubscription(
	ctx context.Context,
	principal attestation.Principal,
) (bool, error) {
	if checker == nil || checker.store == nil || principal.KeyID == "" {
		return false, nil
	}
	record, ok, err := checker.store.Get(ctx, principal.KeyID)
	if err != nil || !ok {
		return false, err
	}
	return checker.now().Before(record.ExpiresAt), nil
}

func NewDevelopmentService(
	appID string,
	store Store,
	duration time.Duration,
	now func() time.Time,
) *DevelopmentService {
	if now == nil {
		now = time.Now
	}
	return &DevelopmentService{
		store:       store,
		appID:       appID,
		duration:    duration,
		now:         now,
		environment: "development",
	}
}

func (service *DevelopmentService) Activate(
	ctx context.Context,
	principal attestation.Principal,
	_ string,
) (time.Time, error) {
	if service == nil || service.store == nil || service.appID == "" || principal.AppID != service.appID ||
		principal.KeyID == "" || principal.DeviceID == "" || principal.TransactionID != "" ||
		service.duration <= 0 || service.duration > 30*24*time.Hour {
		return time.Time{}, ErrActivationDenied
	}
	existing, found, err := service.store.Get(ctx, principal.KeyID)
	if err != nil {
		return time.Time{}, err
	}
	if found && existing.Environment != service.environment {
		return time.Time{}, ErrActivationDenied
	}
	expiresAt := service.now().Add(service.duration)
	record := Record{
		KeyID:       principal.KeyID,
		ExpiresAt:   expiresAt,
		StartedAt:   service.now(),
		Environment: service.environment,
	}
	if err := service.store.Upsert(ctx, record); err != nil {
		return time.Time{}, err
	}
	return expiresAt, nil
}

func (service *DevelopmentService) HasManagedSubscription(
	ctx context.Context,
	principal attestation.Principal,
) (bool, error) {
	if service == nil {
		return false, nil
	}
	return NewChecker(service.store, service.now).HasManagedSubscription(ctx, principal)
}
