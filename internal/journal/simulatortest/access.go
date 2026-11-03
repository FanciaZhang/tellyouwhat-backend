package simulatortest

import (
	"context"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

// Access substitutes only the explicitly provisioned test subscription source.
// Consent, character accounting, provider calls and project budgets stay real.
type Access struct {
	Simulator *Service
	Normal    *readaloud.Access
}

func (a Access) credential(key string) (Credential, error) {
	c, err := a.Simulator.Lookup(key)
	if err != nil {
		return c, readaloud.ErrAccessUnavailable
	}
	if c.SubscriptionStartedAt == nil {
		return c, readaloud.ErrSubscriptionRequired
	}
	return c, nil
}

func (a Access) Subject(ctx context.Context, key string) (readaloud.Subject, error) {
	if !IsKey(key) {
		return a.Normal.Subject(ctx, key)
	}
	c, err := a.credential(key)
	if err != nil {
		return readaloud.Subject{}, err
	}
	return readaloud.Subject{AppID: "journal", ID: "simulator:" + c.SubjectID}, nil
}

func (a Access) Consent(ctx context.Context, key, purpose string) error {
	if IsKey(key) {
		if _, err := a.Simulator.Lookup(key); err != nil {
			return readaloud.ErrAccessUnavailable
		}
	}
	return a.Normal.Consent(ctx, key, purpose)
}

func (a Access) Authorize(ctx context.Context, key readaloud.Subject, purpose string) error {
	if !IsKey(key.ID) {
		return a.Normal.Authorize(ctx, key, purpose)
	}
	if key.AppID != "journal" {
		return readaloud.ErrInput
	}
	if _, err := a.Grant(ctx, key.ID, purpose); err != nil {
		return err
	}
	return a.Consent(ctx, key.ID, purpose)
}

func (a Access) Grant(ctx context.Context, key, purpose string) (readaloud.Grant, error) {
	if !IsKey(key) {
		return a.Normal.Grant(ctx, key, purpose)
	}
	c, err := a.credential(key)
	if err != nil {
		return readaloud.Grant{}, err
	}
	now := a.Simulator.now().UTC()
	if !now.Before(*c.SubscriptionExpiresAt) {
		return readaloud.Grant{}, readaloud.ErrSubscriptionRequired
	}
	if c.SubscriptionStartedAt.After(now) {
		return readaloud.Grant{}, readaloud.ErrSubscriptionSync
	}
	start, end := voice.Period(*c.SubscriptionStartedAt, now)
	limit := c.JournalCharacters
	if purpose == "preview" {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end = start.Add(24 * time.Hour)
		limit = c.PreviewCharacters
	}
	g := readaloud.Grant{Subject: readaloud.Subject{AppID: "journal", ID: "simulator:" + c.SubjectID}, Purpose: purpose, Start: start, End: end, Limit: limit}
	if !g.Valid(now) {
		return readaloud.Grant{}, readaloud.ErrInput
	}
	return g, nil
}
