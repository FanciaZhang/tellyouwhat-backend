package entitlement

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

var ErrActivationDenied = errors.New("development entitlement activation denied")

type Record struct {
	KeyID              string
	TransactionID      string
	ExpiresAt          time.Time
	StartedAt          time.Time
	Environment        string
	OfferTransactionID string
	OfferIdentifier    string
	OfferType          int32
	OfferSignedAt      time.Time
}

type Store interface {
	Upsert(context.Context, Record) error
	Get(context.Context, string) (Record, bool, error)
}

type DevelopmentService struct {
	store       Store
	secret      []byte
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
	store Store,
	secret string,
	duration time.Duration,
	now func() time.Time,
) *DevelopmentService {
	if now == nil {
		now = time.Now
	}
	return &DevelopmentService{
		store:       store,
		secret:      []byte(secret),
		duration:    duration,
		now:         now,
		environment: "development",
	}
}

func (service *DevelopmentService) Activate(
	ctx context.Context,
	principal attestation.Principal,
	secret string,
) (time.Time, error) {
	if service == nil || service.store == nil || len(service.secret) == 0 || principal.KeyID == "" ||
		len(secret) != len(service.secret) || subtle.ConstantTimeCompare([]byte(secret), service.secret) != 1 {
		return time.Time{}, ErrActivationDenied
	}
	expiresAt := service.now().Add(service.duration)
	record := Record{
		KeyID:         principal.KeyID,
		TransactionID: principal.TransactionID,
		ExpiresAt:     expiresAt,
		StartedAt:     service.now(),
		Environment:   service.environment,
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
