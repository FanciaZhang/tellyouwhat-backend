// Created by OpenAI Codex on 2026-08-03.

package entitlement

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

var (
	ErrProductionSyncDenied    = errors.New("production entitlement sync denied")
	ErrSubscriptionInactive    = errors.New("managed subscription is inactive")
	ErrSubscriptionUnavailable = errors.New("managed subscription verification unavailable")
)

type SubscriptionState struct {
	OriginalTransactionID string
	TransactionID         string
	Environment           string
	OfferIdentifier       string
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
	if service == nil || service.store == nil || service.resolver == nil || service.binder == nil || principal.KeyID == "" ||
		strings.TrimSpace(signedTransaction) == "" {
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
		KeyID:              principal.KeyID,
		TransactionID:      state.OriginalTransactionID,
		ExpiresAt:          state.ExpiresAt,
		StartedAt:          state.StartedAt,
		Environment:        environment,
		OfferTransactionID: state.TransactionID,
		OfferIdentifier:    state.OfferIdentifier,
		OfferType:          state.OfferType,
		OfferSignedAt:      state.SignedAt,
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
