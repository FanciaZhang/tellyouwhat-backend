// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	statusActive      = 1
	statusGracePeriod = 4
)

var (
	ErrSubscriptionInactive = errors.New("app store subscription is inactive")
	ErrStatusUnavailable    = errors.New("app store subscription status is unavailable")
)

type SubscriptionStatus struct {
	Status            int
	SignedTransaction string
	SignedRenewal     string
}

type StatusClient interface {
	CurrentTransactions(context.Context, string) ([]SubscriptionStatus, error)
}

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

type SubscriptionResolver struct {
	verifier *TransactionVerifier
	client   StatusClient
	now      func() time.Time
}

func NewSubscriptionResolver(
	verifier *TransactionVerifier,
	client StatusClient,
	now func() time.Time,
) *SubscriptionResolver {
	if now == nil {
		now = time.Now
	}
	return &SubscriptionResolver{verifier: verifier, client: client, now: now}
}

func (resolver *SubscriptionResolver) Resolve(
	ctx context.Context,
	signedTransaction string,
) (SubscriptionState, error) {
	if resolver == nil || resolver.verifier == nil || resolver.client == nil {
		return SubscriptionState{}, ErrStatusUnavailable
	}
	submitted, err := resolver.verifier.VerifyTransaction(signedTransaction)
	if err != nil {
		return SubscriptionState{}, err
	}
	statuses, err := resolver.client.CurrentTransactions(ctx, submitted.TransactionID)
	if err != nil {
		if errors.Is(err, ErrTransactionNotFound) {
			return SubscriptionState{}, ErrTransactionNotFound
		}
		return SubscriptionState{}, fmt.Errorf("%w: %v", ErrStatusUnavailable, err)
	}
	var active Transaction
	for _, status := range statuses {
		transaction, verifyErr := resolver.verifier.VerifyTransaction(status.SignedTransaction)
		if verifyErr != nil {
			return SubscriptionState{}, verifyErr
		}
		if transaction.OriginalTransactionID != submitted.OriginalTransactionID ||
			transaction.RevokedAt != nil || transaction.IsUpgraded {
			continue
		}
		expiresAt := transaction.ExpiresAt
		if status.Status == statusGracePeriod {
			renewal, renewalErr := resolver.verifier.VerifyRenewal(status.SignedRenewal)
			if renewalErr != nil {
				return SubscriptionState{}, renewalErr
			}
			if renewal.OriginalTransactionID != submitted.OriginalTransactionID {
				continue
			}
			expiresAt = renewal.GracePeriodExpiresAt
		} else if status.Status != statusActive {
			continue
		}
		if !resolver.now().Before(expiresAt) {
			continue
		}
		transaction.ExpiresAt = expiresAt
		if active.ExpiresAt.Before(expiresAt) {
			active = transaction
		}
	}
	if active.OriginalTransactionID == "" {
		return SubscriptionState{}, ErrSubscriptionInactive
	}
	return SubscriptionState{
		OriginalTransactionID: active.OriginalTransactionID,
		TransactionID:         active.TransactionID,
		Environment:           active.Environment,
		OfferIdentifier:       active.OfferIdentifier,
		OfferType:             active.OfferType,
		ExpiresAt:             active.ExpiresAt,
		StartedAt:             active.StartedAt,
		SignedAt:              active.SignedAt,
	}, nil
}
