package entitlement

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
)

func TestControlledDevelopmentAccessPreservesPurchaseAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	keys := attestation.NewMemoryKeyStore()
	if err := keys.Register(ctx, attestation.RegisteredKey{AppID: "journal", KeyID: "key", DeviceID: "device"}); err != nil {
		t.Fatal(err)
	}
	purchases := NewMemoryStore()
	expired := Record{KeyID: "key", Environment: "sandbox", TransactionID: "apple-purchase", StartedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if err := purchases.Upsert(ctx, expired); err != nil {
		t.Fatal(err)
	}
	grant := DevelopmentGrant{KeyID: "key", DeviceID: "device", StartedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	grants := []DevelopmentGrant{grant}
	access := &DevelopmentAccess{AppID: "journal", Purchases: purchases, Keys: keys, Grants: func() ([]DevelopmentGrant, error) { return grants, nil }, Now: func() time.Time { return now }}
	p := attestation.Principal{AppID: "journal", KeyID: "key", DeviceID: "device", TransactionID: "apple-purchase"}
	for range 2 {
		if end, err := access.Activate(ctx, p, "app-attest"); err != nil || !end.Equal(grant.ExpiresAt) {
			t.Fatal("activation changed bounded expiry", end, err)
		}
		if ok, err := access.HasManagedSubscription(ctx, p); err != nil || !ok {
			t.Fatal("controlled grant unavailable", err)
		}
	}
	r, ok, err := access.Get(ctx, "key")
	if err != nil || !ok || r.Environment != "development" || r.TransactionID != "" || r.Payment.HasPrice() || !r.StartedAt.Equal(grant.StartedAt) {
		t.Fatal("grant forged a purchase or lost its quota anchor", r, err)
	}
	retained, _, _ := purchases.Get(ctx, "key")
	if retained != expired {
		t.Fatal("grant replaced an Apple purchase")
	}
	for _, invalid := range []attestation.Principal{{AppID: "health", KeyID: "key", DeviceID: "device"}, {AppID: "journal", KeyID: "unknown", DeviceID: "device"}, {AppID: "journal", KeyID: "key", DeviceID: "wrong"}} {
		if _, err := access.Activate(ctx, invalid, "app-attest"); err == nil {
			t.Fatal("unapproved principal activated")
		}
	}
	grants = nil
	if allowed, _ := access.HasManagedSubscription(ctx, p); allowed {
		t.Fatal("revoked grant stayed active")
	}
	grants = []DevelopmentGrant{grant}
	now = grant.ExpiresAt
	if allowed, _ := access.HasManagedSubscription(ctx, p); allowed {
		t.Fatal("expired grant stayed active")
	}
	if _, err := access.Activate(ctx, p, "app-attest"); !errors.Is(err, ErrActivationDenied) {
		t.Fatal("activation extended expiry")
	}
	active := expired
	active.ExpiresAt = now.Add(time.Hour)
	if err := purchases.Upsert(ctx, active); err != nil {
		t.Fatal(err)
	}
	r, _, _ = access.Get(ctx, "key")
	if r != active {
		t.Fatal("verified purchase did not take precedence")
	}
}

func TestDevelopmentGrantRegistryRejectsUnboundedPolicy(t *testing.T) {
	now := time.Now().UTC()
	valid := DevelopmentGrant{KeyID: "key", DeviceID: "device", StartedAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, raw := range []string{`null`, `[]`, `[{}]`, `[{"allDevices":true}]`} {
		if _, err := ParseDevelopmentGrants(raw); err == nil {
			t.Fatal("invalid policy accepted", raw)
		}
	}
	for _, grants := range [][]DevelopmentGrant{{valid}, {valid, valid}, {{KeyID: "key", DeviceID: "device", StartedAt: now, ExpiresAt: now.Add(31 * 24 * time.Hour)}}} {
		raw, _ := json.Marshal(grants)
		_, err := ParseDevelopmentGrants(string(raw))
		if (err == nil) != (len(grants) == 1 && grants[0].ExpiresAt.Sub(now) <= 30*24*time.Hour) {
			t.Fatal("invalid registry validation", err)
		}
	}
}
