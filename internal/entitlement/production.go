// Created by OpenAI Codex on 2026-08-03.

package entitlement

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/purchase"
)

var (
	ErrProductionSyncDenied        = errors.New("production entitlement sync denied")
	ErrSubscriptionInactive        = errors.New("managed subscription is inactive")
	ErrSubscriptionUnavailable     = errors.New("managed subscription verification unavailable")
	ErrSubscriptionBindingConflict = errors.New("managed subscription binding conflict")
)

type SubscriptionState struct {
	Payment               purchase.Evidence
	OriginalTransactionID string
	TransactionID         string
	Environment           string
	OfferIdentifier       string
	ProductID             string
	OfferType             int32
	ExpiresAt             time.Time
	StartedAt             time.Time
	SignedAt              time.Time
}

type SubscriptionResolver interface {
	Resolve(context.Context, string) (SubscriptionState, error)
}

type SubscriptionResolverFunc func(context.Context, string) (SubscriptionState, error)

func (function SubscriptionResolverFunc) Resolve(
	ctx context.Context,
	signedTransaction string,
) (SubscriptionState, error) {
	return function(ctx, signedTransaction)
}

type ProductionService struct {
	store    Store
	resolver SubscriptionResolver
	binder   TransactionBinder
	now      func() time.Time
}

type TransactionBinder interface {
	BindTransaction(context.Context, string, string) error
}

// VerifiedTransactionStore commits the verified entitlement and device binding
// together while retaining a separate purchase binding for each StoreKit environment.
type VerifiedTransactionStore interface {
	UpsertVerified(context.Context, Record) error
}

// VerifiedTransactionBindings keeps environment switches from replacing an already
// bound purchase in the other environment.
type VerifiedTransactionBindings struct {
	ProductionID string
	SandboxID    string
}

func BindVerifiedTransaction(boundID string, existing Record, incoming Record, bindings VerifiedTransactionBindings) (VerifiedTransactionBindings, bool) {
	if existing.TransactionID != boundID {
		return bindings, false
	}
	bind := func(id, environment string) bool {
		if id == "" {
			return false
		}
		var anchor *string
		switch environment {
		case "production":
			anchor = &bindings.ProductionID
		case "sandbox":
			anchor = &bindings.SandboxID
		default:
			return false
		}
		if *anchor != "" && *anchor != id {
			return false
		}
		*anchor = id
		return true
	}
	// Existing verified records seed bindings for devices enrolled before the
	// environment-specific anchors were introduced.
	if boundID != "" && !bind(boundID, existing.Environment) {
		return bindings, false
	}
	if !bind(incoming.TransactionID, incoming.Environment) {
		return bindings, false
	}
	return bindings, true
}

func NewProductionService(store Store, resolver SubscriptionResolver, now func() time.Time) *ProductionService {
	if now == nil {
		now = time.Now
	}
	return &ProductionService{store: store, resolver: resolver, now: now}
}

func (service *ProductionService) WithTransactionBinder(binder TransactionBinder) *ProductionService {
	service.binder = binder
	return service
}

func (service *ProductionService) Sync(
	ctx context.Context,
	principal attestation.Principal,
	signedTransaction string,
) (time.Time, error) {
	if service == nil || service.store == nil || service.resolver == nil || principal.KeyID == "" ||
		strings.TrimSpace(signedTransaction) == "" {
		return time.Time{}, ErrProductionSyncDenied
	}
	verifiedStore, atomicBinding := service.store.(VerifiedTransactionStore)
	if !atomicBinding && service.binder == nil {
		return time.Time{}, ErrProductionSyncDenied
	}
	state, err := service.resolver.Resolve(ctx, signedTransaction)
	if err != nil {
		return time.Time{}, err
	}
	environment := strings.ToLower(strings.TrimSpace(state.Environment))
	if state.OriginalTransactionID == "" || (environment != "production" && environment != "sandbox") {
		return time.Time{}, ErrProductionSyncDenied
	}
	if !service.now().Before(state.ExpiresAt) {
		return time.Time{}, ErrSubscriptionInactive
	}
	record := Record{
		Payment:            state.Payment,
		KeyID:              principal.KeyID,
		TransactionID:      state.OriginalTransactionID,
		ExpiresAt:          state.ExpiresAt,
		StartedAt:          state.StartedAt,
		Environment:        environment,
		OfferTransactionID: state.TransactionID,
		OfferIdentifier:    state.OfferIdentifier,
		ProductID:          state.ProductID,
		OfferType:          state.OfferType,
		OfferSignedAt:      state.SignedAt,
	}
	if atomicBinding {
		if err := verifiedStore.UpsertVerified(ctx, record); err != nil {
			return time.Time{}, err
		}
		return state.ExpiresAt, nil
	}
	if err := service.binder.BindTransaction(ctx, principal.KeyID, state.OriginalTransactionID); err != nil {
		if errors.Is(err, attestation.ErrTransactionBindingConflict) {
			return time.Time{}, ErrProductionSyncDenied
		}
		return time.Time{}, err
	}
	if err := service.store.Upsert(ctx, record); err != nil {
		return time.Time{}, err
	}
	return state.ExpiresAt, nil
}
