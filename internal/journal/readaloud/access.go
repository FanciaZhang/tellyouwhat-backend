package readaloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/privacy"
)

var ErrAccessUnavailable = errors.New("read-aloud authorization unavailable")

type EntitlementReader interface {
	Get(context.Context, string) (entitlement.Record, bool, error)
}
type ConsentReader interface {
	HasRequiredConsents(context.Context, attestation.Principal, []string) (bool, error)
}
type Access struct {
	Entitlements      EntitlementReader
	Consents          ConsentReader
	JournalCharacters int
	PreviewCharacters int
	Now               func() time.Time
}

func (a Access) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
func (a Access) record(ctx context.Context, key string) (entitlement.Record, error) {
	if a.Entitlements == nil || !subjectPattern.MatchString(key) {
		return entitlement.Record{}, ErrAccessUnavailable
	}
	r, ok, err := a.Entitlements.Get(ctx, key)
	if err != nil {
		return r, ErrAccessUnavailable
	}
	if !ok || r.TransactionID == "" || (r.Environment != "production" && r.Environment != "sandbox") {
		return r, ErrSubscriptionRequired
	}
	return r, nil
}
func purchaseSubject(r entitlement.Record) Subject {
	hash := sha256.Sum256([]byte(r.Environment + "\x00" + r.TransactionID))
	return Subject{AppID: "journal", ID: "purchase:" + hex.EncodeToString(hash[:])}
}

// Completed audio and cancellation remain reachable after subscription expiry.
func (a Access) Subject(ctx context.Context, key string) (Subject, error) {
	r, err := a.record(ctx, key)
	if err != nil {
		return Subject{}, err
	}
	return purchaseSubject(r), nil
}
func (a Access) Consent(ctx context.Context, key, purpose string) error {
	if purpose == "preview" {
		return nil
	}
	if purpose != "journal" {
		return ErrInput
	}
	if a.Consents == nil {
		return ErrAccessUnavailable
	}
	ok, err := a.Consents.HasRequiredConsents(ctx, attestation.Principal{AppID: "journal", KeyID: key}, []string{privacy.JournalReadAloudScope})
	if err != nil {
		return ErrAccessUnavailable
	}
	if !ok {
		return ErrConsentRequired
	}
	return nil
}

// Session authorization uses its authenticated device key, while the character
// account uses the purchase identity shared by that subscriber's devices.
func (a Access) Authorize(ctx context.Context, key Subject, purpose string) error {
	if !key.Valid() {
		return ErrInput
	}
	r, err := a.record(ctx, key.ID)
	if err != nil {
		return err
	}
	if !a.now().Before(r.ExpiresAt) {
		return ErrSubscriptionRequired
	}
	return a.Consent(ctx, key.ID, purpose)
}
func (a Access) Grant(ctx context.Context, key, purpose string) (Grant, error) {
	r, err := a.record(ctx, key)
	if err != nil {
		return Grant{}, err
	}
	now := a.now().UTC()
	if !now.Before(r.ExpiresAt) {
		return Grant{}, ErrSubscriptionRequired
	}
	if r.StartedAt.IsZero() || r.StartedAt.After(now) {
		return Grant{}, ErrSubscriptionSync
	}
	start, end := voice.Period(r.StartedAt, now)
	limit := a.JournalCharacters
	if purpose == "preview" {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.Add(24 * time.Hour)
		limit = a.PreviewCharacters
	}
	g := Grant{Subject: purchaseSubject(r), Purpose: purpose, Start: start, End: end, Limit: limit}
	if !g.Valid(now) {
		return Grant{}, ErrInput
	}
	return g, nil
}
