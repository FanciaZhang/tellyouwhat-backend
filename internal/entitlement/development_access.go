package entitlement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

// Development grants are operator-owned configuration, never Apple purchase rows.
type DevelopmentGrant struct {
	KeyID     string    `json:"keyID"`
	DeviceID  string    `json:"deviceID"`
	StartedAt time.Time `json:"startedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func ParseDevelopmentGrants(raw string) ([]DevelopmentGrant, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("invalid bounded development grants")
	if len(raw) > 8192 {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var grants []DevelopmentGrant
	if decoder.Decode(&grants) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(grants) == 0 || len(grants) > 16 {
		return nil, invalid
	}
	keys := map[string]bool{}
	for _, grant := range grants {
		if grant.KeyID == "" || len(grant.KeyID) > 512 || grant.DeviceID == "" || len(grant.DeviceID) > 128 || keys[grant.KeyID] ||
			grant.StartedAt.IsZero() || !grant.StartedAt.Before(grant.ExpiresAt) || grant.ExpiresAt.Sub(grant.StartedAt) > 30*24*time.Hour {
			return nil, invalid
		}
		keys[grant.KeyID] = true
	}
	return grants, nil
}

type DevelopmentAccess struct {
	AppID     string
	Purchases Reader
	Keys      attestation.KeyStore
	Grants    func() ([]DevelopmentGrant, error)
	Now       func() time.Time
}

func (access *DevelopmentAccess) now() time.Time {
	if access.Now != nil {
		return access.Now()
	}
	return time.Now()
}

func (access *DevelopmentAccess) grant(ctx context.Context, keyID string) (DevelopmentGrant, bool, error) {
	if access == nil || access.Keys == nil || access.Grants == nil || access.AppID == "" {
		return DevelopmentGrant{}, false, nil
	}
	grants, err := access.Grants()
	if err != nil {
		return DevelopmentGrant{}, false, err
	}
	for _, grant := range grants {
		if grant.KeyID != keyID {
			continue
		}
		key, err := access.Keys.Get(ctx, keyID)
		if err != nil {
			return DevelopmentGrant{}, false, err
		}
		valid := key.AppID == access.AppID && key.DeviceID == grant.DeviceID &&
			!access.now().Before(grant.StartedAt) && access.now().Before(grant.ExpiresAt)
		return grant, valid, nil
	}
	return DevelopmentGrant{}, false, nil
}

func (access *DevelopmentAccess) Activate(ctx context.Context, principal attestation.Principal, _ string) (time.Time, error) {
	if principal.AppID != access.AppID || principal.DeviceID == "" {
		return time.Time{}, ErrActivationDenied
	}
	grant, ok, err := access.grant(ctx, principal.KeyID)
	if err != nil {
		return time.Time{}, err
	}
	if !ok || grant.DeviceID != principal.DeviceID {
		return time.Time{}, ErrActivationDenied
	}
	return grant.ExpiresAt, nil
}

func (access *DevelopmentAccess) Get(ctx context.Context, keyID string) (Record, bool, error) {
	record, found, err := access.Purchases.Get(ctx, keyID)
	if err != nil {
		return Record{}, false, err
	}
	if found && record.TransactionID != "" && (record.Environment == "sandbox" || record.Environment == "production") && access.now().Before(record.ExpiresAt) {
		return record, true, nil
	}
	grant, ok, err := access.grant(ctx, keyID)
	if err != nil {
		return Record{}, false, err
	}
	if ok {
		return Record{KeyID: keyID, Environment: "development", StartedAt: grant.StartedAt, ExpiresAt: grant.ExpiresAt}, true, nil
	}
	if record.Environment == "development" {
		return Record{}, false, nil
	}
	return record, found, nil
}

func (access *DevelopmentAccess) HasManagedSubscription(ctx context.Context, principal attestation.Principal) (bool, error) {
	if principal.AppID != access.AppID {
		return false, nil
	}
	record, found, err := access.Get(ctx, principal.KeyID)
	return found && access.now().Before(record.ExpiresAt) && (record.Environment == "development" ||
		((record.Environment == "sandbox" || record.Environment == "production") && record.TransactionID != "")), err
}
