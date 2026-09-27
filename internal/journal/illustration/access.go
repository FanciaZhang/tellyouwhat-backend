package illustration

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/privacy"
)

var ErrAccessDenied = errors.New("illustration access unavailable or revoked")

type RegisteredKeyReader interface {
	Get(context.Context, string) (attestation.RegisteredKey, error)
}
type SubscriptionReader interface {
	Get(context.Context, string) (entitlement.Record, bool, error)
}
type ReservationReader interface {
	HasImageReservation(context.Context, Task) (bool, error)
}

// AccessPolicy is Journal-only and reads current persisted grants on every call.
// Configure readers scoped to Journal; never accept a client-supplied allowance.
type AccessPolicy struct {
	Keys          RegisteredKeyReader
	Subscriptions SubscriptionReader
	Consents      privacy.ConsentReader
	Reservations  ReservationReader
	MonthlyLimit  int64
	Now           func() time.Time
}

func (p *AccessPolicy) ImageAllowance(ctx context.Context, owner string, now time.Time) (string, int64, error) {
	if p == nil || p.Keys == nil || p.Subscriptions == nil || p.Consents == nil || p.MonthlyLimit < 1 || owner == "" || now.IsZero() {
		return "", 0, ErrAccessDenied
	}
	key, err := p.Keys.Get(ctx, owner)
	if err != nil {
		return "", 0, err
	}
	if key.AppID != "journal" || key.KeyID != owner || strings.TrimSpace(key.TransactionID) == "" || len(key.TransactionID) > 255 {
		return "", 0, ErrAccessDenied
	}
	subscription, exists, err := p.Subscriptions.Get(ctx, owner)
	if err != nil {
		return "", 0, err
	}
	if !exists || subscription.KeyID != owner || subscription.TransactionID != key.TransactionID || !now.Before(subscription.ExpiresAt) || now.Before(subscription.StartedAt) {
		return "", 0, ErrAccessDenied
	}
	allowed, err := p.Consents.HasGrantedConsents(ctx, owner, []privacy.Consent{
		{Scope: privacy.ManagedAIScope, DocumentVersion: privacy.AIDocumentVersion, Granted: true},
		{Scope: privacy.JournalIllustrationScope, DocumentVersion: privacy.JournalIllustrationDocumentVersion, Granted: true},
	})
	if err != nil {
		return "", 0, err
	}
	if !allowed {
		return "", 0, ErrAccessDenied
	}
	return key.TransactionID, p.MonthlyLimit, nil
}

func (p *AccessPolicy) Authorize(ctx context.Context, task Task) error {
	if p == nil || p.Reservations == nil {
		return ErrAccessDenied
	}
	if err := p.AuthorizeRead(ctx, task); err != nil {
		return err
	}
	reserved, err := p.Reservations.HasImageReservation(ctx, task)
	if err != nil {
		return err
	}
	if !reserved {
		return ErrAccessDenied
	}
	return nil
}

// AuthorizeRead rechecks live consent and account binding without requiring a
// reservation: successful tasks have already consumed their reservation.
func (p *AccessPolicy) AuthorizeRead(ctx context.Context, task Task) error {
	if p == nil || task.AppID != "journal" || task.ConsentRevision != privacy.JournalIllustrationDocumentVersion {
		return ErrAccessDenied
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	account, _, err := p.ImageAllowance(ctx, task.OwnerID, now)
	if err != nil {
		return err
	}
	if account != task.BillingAccount || !now.Before(task.ExpiresAt) || now.Before(task.CreatedAt) {
		return ErrAccessDenied
	}
	return nil
}
