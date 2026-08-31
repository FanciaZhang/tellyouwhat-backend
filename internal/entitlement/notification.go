// Created by OpenAI Codex on 2026-08-03.

package entitlement

import (
	"context"
	"strings"
	"time"
)

type NotificationState struct {
	NotificationUUID      string
	OriginalTransactionID string
	Environment           string
	TransactionID         string
	OfferIdentifier       string
	OfferType             int32
	SignedAt              time.Time
	ExpiresAt             time.Time
}

type NotificationResolver interface {
	ResolveNotification(context.Context, string) (NotificationState, error)
}

type NotificationResolverFunc func(context.Context, string) (NotificationState, error)

func (function NotificationResolverFunc) ResolveNotification(
	ctx context.Context,
	signedPayload string,
) (NotificationState, error) {
	return function(ctx, signedPayload)
}

type NotificationStore interface {
	ApplyNotification(context.Context, NotificationState) (bool, error)
}

type NotificationService struct {
	store    NotificationStore
	resolver NotificationResolver
}

func NewNotificationService(store NotificationStore, resolver NotificationResolver) *NotificationService {
	return &NotificationService{store: store, resolver: resolver}
}

func (service *NotificationService) Process(
	ctx context.Context,
	signedPayload string,
) (bool, error) {
	if service == nil || service.store == nil || service.resolver == nil || strings.TrimSpace(signedPayload) == "" {
		return false, ErrProductionSyncDenied
	}
	state, err := service.resolver.ResolveNotification(ctx, signedPayload)
	if err != nil {
		return false, err
	}
	environment := strings.ToLower(strings.TrimSpace(state.Environment))
	if state.NotificationUUID == "" || state.OriginalTransactionID == "" ||
		(environment != "production" && environment != "sandbox") || state.ExpiresAt.IsZero() {
		return false, ErrProductionSyncDenied
	}
	state.Environment = environment
	return service.store.ApplyNotification(ctx, state)
}
