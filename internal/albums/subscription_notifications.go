package albums

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Created only after verifying Apple's notification and nested transaction.
// Fingerprints exclude signatures (which may be reissued) and contain no media.
type albumNotificationReceipt struct {
	ID, Kind, Environment, Owner, Original string
	SignedAt                               time.Time
	Digest                                 [32]byte
}
type albumNotificationRepository interface {
	NotificationProcessed(context.Context, albumNotificationReceipt) (bool, error)
	ApplySubscriptionNotification(context.Context, albumNotificationReceipt, int64, VerifiedAlbumSubscription, time.Time) error
	RecordSubscriptionTestNotification(context.Context, albumNotificationReceipt, time.Time) error
}

func (s *AlbumSubscriptionService) receiveNotification(ctx context.Context, signed string) error {
	if s == nil || s.resolver == nil {
		return ErrSubscriptionUnavailable
	}
	repository, ok := s.repository.(albumNotificationRepository)
	if !ok {
		return ErrSubscriptionUnavailable
	}
	n, err := s.resolver.verifier.VerifyNotification(signed)
	if err != nil {
		return ErrSubscriptionVerification
	}
	id, err := uuid.Parse(n.NotificationUUID)
	if err != nil || id == uuid.Nil || !strings.EqualFold(n.NotificationUUID, id.String()) ||
		!regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`).MatchString(n.NotificationType) || n.SignedAt.Year() < 2000 || n.SignedAt.Year() > 9998 {
		return ErrSubscriptionVerification
	}
	receipt := albumNotificationReceipt{ID: id.String(), Kind: n.NotificationType, Environment: s.resolver.environment, SignedAt: n.SignedAt.UTC()}
	semantic := []any{receipt.ID, receipt.Kind, receipt.Environment, receipt.SignedAt.UnixMilli()}
	var transactionID string
	if n.NotificationType != "TEST" {
		t, err := s.resolver.verifier.VerifyTransaction(n.SignedTransactionInfo)
		if err != nil {
			return ErrSubscriptionVerification
		}
		token, err := uuid.Parse(t.AppAccountToken)
		if err != nil || !validOwner(token.String()) || !transactionBelongsToAccount(t, token.String(), s.resolver.environment) {
			return ErrSubscriptionBinding
		}
		receipt.Owner, err = s.repository.SubscriptionOwner(ctx, token.String())
		if err != nil {
			return err
		}
		receipt.Original, transactionID = t.OriginalTransactionID, t.TransactionID
		var revokedAt int64
		if t.RevokedAt != nil {
			revokedAt = t.RevokedAt.UnixMilli()
		}
		semantic = append(semantic, receipt.Owner, receipt.Original, t.TransactionID, t.ProductID, t.SignedAt.UnixMilli(), t.ExpiresAt.UnixMilli(), revokedAt, t.IsUpgraded)
	}
	data, err := json.Marshal(semantic)
	if err != nil {
		return ErrSubscriptionVerification
	}
	receipt.Digest = sha256.Sum256(data)
	done, err := repository.NotificationProcessed(ctx, receipt)
	if err != nil || done {
		return err
	}
	if receipt.Kind == "TEST" {
		return repository.RecordSubscriptionTestNotification(ctx, receipt, s.now().UTC())
	}
	// A signed event is a trigger, not a client-selected current status. Even a
	// delayed refund/renewal event resolves the latest Apple state before apply.
	for attempt := 0; attempt < 2; attempt++ {
		before, err := s.repository.CloudAccount(ctx, receipt.Owner, s.now().UTC())
		if err != nil {
			return err
		}
		v, err := s.resolver.resolveCurrent(ctx, receipt.Owner, receipt.Original, transactionID, before.AppAccountToken)
		if err != nil {
			return err
		}
		err = repository.ApplySubscriptionNotification(ctx, receipt, before.Revision, v, s.now().UTC())
		if !errors.Is(err, ErrSubscriptionConflict) {
			return err
		}
	}
	return ErrSubscriptionConflict
}
