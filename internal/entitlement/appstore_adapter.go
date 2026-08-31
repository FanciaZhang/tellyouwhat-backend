// Created by OpenAI Codex on 2026-08-03.

package entitlement

import (
	"context"
	"errors"

	"github.com/tellyouwhat/backend/internal/appstore"
)

type appStoreSubscriptionSource interface {
	Resolve(context.Context, string) (appstore.SubscriptionState, error)
}

type appStoreNotificationSource interface {
	Process(context.Context, string) (appstore.NotificationResult, error)
}

type appStoreSubscriptionAdapter struct {
	source appStoreSubscriptionSource
}

type appStoreNotificationAdapter struct {
	source appStoreNotificationSource
}

func NewAppStoreSubscriptionResolver(source appStoreSubscriptionSource) SubscriptionResolver {
	return &appStoreSubscriptionAdapter{source: source}
}

func NewAppStoreNotificationResolver(source appStoreNotificationSource) NotificationResolver {
	return &appStoreNotificationAdapter{source: source}
}

func (adapter *appStoreSubscriptionAdapter) Resolve(
	ctx context.Context,
	signedTransaction string,
) (SubscriptionState, error) {
	if adapter == nil || adapter.source == nil {
		return SubscriptionState{}, ErrSubscriptionUnavailable
	}
	state, err := adapter.source.Resolve(ctx, signedTransaction)
	if err != nil {
		return SubscriptionState{}, mapAppStoreError(err)
	}
	return SubscriptionState{
		OriginalTransactionID: state.OriginalTransactionID,
		TransactionID:         state.TransactionID,
		Environment:           state.Environment,
		OfferIdentifier:       state.OfferIdentifier,
		OfferType:             state.OfferType,
		ExpiresAt:             state.ExpiresAt,
		SignedAt:              state.SignedAt,
	}, nil
}

func (adapter *appStoreNotificationAdapter) ResolveNotification(
	ctx context.Context,
	signedPayload string,
) (NotificationState, error) {
	if adapter == nil || adapter.source == nil {
		return NotificationState{}, ErrSubscriptionUnavailable
	}
	state, err := adapter.source.Process(ctx, signedPayload)
	if err != nil {
		mapped := mapAppStoreError(err)
		if errors.Is(mapped, ErrSubscriptionInactive) {
			mapped = ErrSubscriptionUnavailable
		}
		return NotificationState{}, mapped
	}
	return NotificationState{
		NotificationUUID:      state.NotificationUUID,
		OriginalTransactionID: state.OriginalTransactionID,
		Environment:           state.Environment,
		TransactionID:         state.TransactionID,
		OfferIdentifier:       state.OfferIdentifier,
		OfferType:             state.OfferType,
		SignedAt:              state.SignedAt,
		ExpiresAt:             state.ExpiresAt,
	}, nil
}

func mapAppStoreError(err error) error {
	switch {
	case errors.Is(err, appstore.ErrInvalidSignedData):
		return ErrProductionSyncDenied
	case errors.Is(err, appstore.ErrSubscriptionInactive), errors.Is(err, appstore.ErrTransactionNotFound):
		return ErrSubscriptionInactive
	default:
		return ErrSubscriptionUnavailable
	}
}
