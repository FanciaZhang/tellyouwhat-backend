package albums

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type albumStatusStub struct {
	calls  int
	states []appstore.SubscriptionStatus
	err    error
}

func (s *albumStatusStub) AllSubscriptionStatuses(context.Context, string) ([]appstore.SubscriptionStatus, error) {
	s.calls++
	return s.states, s.err
}

func TestAlbumSubscriptionUsesCurrentAccountBoundAppleEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	owner := uuid.NewString()
	fixture := newAlbumReceiptFixture(t, now)
	for _, name := range []string{"active-50", "free-trial-active", "free-trial-expired", "upgrade-500", "expired", "retrying", "grace", "revoked", "missing-token", "wrong-submitted-owner", "wrong-current-owner", "tampered", "health-bundle", "health-product", "wrong-environment", "family-shared", "missing-current", "provider-failure", "unknown-status", "future-expired-status", "wrong-renewal", "upgraded-old-transaction"} {
		t.Run(name, func(t *testing.T) {
			submitted, current := fixture.payload(owner), fixture.payload(owner)
			status, renewal := 1, ""
			want := error(nil)
			capacity := int64(50_000_000_000)
			active := true
			expiry := now.AddDate(1, 0, 0)
			switch name {
			case "free-trial-active", "free-trial-expired":
				expiry = now.Add(14 * 24 * time.Hour)
				if name == "free-trial-expired" {
					expiry, status, active = now.Add(-time.Hour), 2, false
				}
				for _, payload := range []map[string]any{submitted, current} {
					payload["price"], payload["currency"] = int64(0), "CNY"
					payload["offerType"], payload["offerDiscountType"] = 1, "FREE_TRIAL"
					payload["originalPurchaseDate"] = expiry.Add(-14 * 24 * time.Hour).UnixMilli()
					payload["purchaseDate"] = payload["originalPurchaseDate"]
					payload["expiresDate"] = expiry.UnixMilli()
				}
			case "upgrade-500":
				current["productId"] = Cloud500GBAnnualProduct
				current["transactionId"] = "1000000000000003"
				capacity = 500_000_000_000
			case "expired", "retrying":
				status = 2
				if name == "retrying" {
					status = 3
				}
				expiry = now.Add(-time.Hour)
				current["expiresDate"] = expiry.UnixMilli()
				current["originalPurchaseDate"] = now.Add(-48 * time.Hour).UnixMilli()
				current["purchaseDate"] = now.Add(-24 * time.Hour).UnixMilli()
				active = false
			case "grace":
				status = 4
				current["expiresDate"] = now.Add(-time.Minute).UnixMilli()
				expiry = now.Add(24 * time.Hour)
				renewal = fixture.sign(t, map[string]any{"originalTransactionId": current["originalTransactionId"], "autoRenewProductId": Cloud50GBAnnualProduct, "environment": "Sandbox", "gracePeriodExpiresDate": expiry.UnixMilli(), "signedDate": now.UnixMilli()})
			case "revoked":
				status = 5
				expiry = now.Add(-time.Minute)
				current["revocationDate"] = expiry.UnixMilli()
				active = false
			case "missing-token":
				delete(submitted, "appAccountToken")
				want = ErrSubscriptionBinding
			case "wrong-submitted-owner":
				submitted["appAccountToken"] = uuid.NewString()
				want = ErrSubscriptionBinding
			case "wrong-current-owner":
				current["appAccountToken"] = uuid.NewString()
				want = ErrSubscriptionBinding
			case "tampered":
				want = ErrSubscriptionVerification
			case "health-bundle":
				submitted["bundleId"] = "cn.tellyouwhat.healthapp"
				want = ErrSubscriptionVerification
			case "health-product":
				submitted["productId"] = "health.premium.subscription.annual"
				want = ErrSubscriptionVerification
			case "wrong-environment":
				submitted["environment"] = "Production"
				want = ErrSubscriptionVerification
			case "family-shared":
				submitted["inAppOwnershipType"] = "FAMILY_SHARED"
				want = ErrSubscriptionBinding
			case "missing-current", "provider-failure", "unknown-status", "future-expired-status", "upgraded-old-transaction":
				want = ErrSubscriptionUnavailable
			case "wrong-renewal":
				status = 4
				renewal = fixture.sign(t, map[string]any{"originalTransactionId": "other", "autoRenewProductId": Cloud50GBAnnualProduct, "environment": "Sandbox", "gracePeriodExpiresDate": now.Add(time.Hour).UnixMilli(), "signedDate": now.UnixMilli()})
				want = ErrSubscriptionVerification
			}
			if name == "unknown-status" {
				status = 6
			}
			if name == "future-expired-status" {
				status = 2
			}
			if name == "upgraded-old-transaction" {
				current["isUpgraded"] = true
			}
			client := &albumStatusStub{states: []appstore.SubscriptionStatus{{Status: status, SignedTransaction: fixture.sign(t, current), SignedRenewal: renewal}}}
			if name == "missing-current" {
				client.states = nil
			}
			if name == "provider-failure" {
				client.err = errors.New("unavailable")
			}
			resolver, err := NewAlbumSubscriptionResolver(fixture.verifier(), client, "Sandbox", func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			signed := fixture.sign(t, submitted)
			if name == "tampered" {
				parts := strings.Split(signed, ".")
				parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"appAccountToken":"` + owner + `"}`))
				signed = strings.Join(parts, ".")
			}
			result, err := resolver.Resolve(context.Background(), owner, signed)
			if want != nil {
				if !errors.Is(err, want) {
					t.Fatal("wrong rejection", err)
				}
				return
			}
			if err != nil || result.Active != active || subscriptionCapacity(result.ProductID) != capacity || !result.ExpiresAt.Equal(expiry) || client.calls != 1 {
				t.Fatal("current entitlement mismatch", result, err)
			}
		})
	}
}

func TestAlbumRetentionUsesSixClampedCalendarMonths(t *testing.T) {
	for _, pair := range [][2]string{{"2026-08-31T23:59:59.123456Z", "2027-02-28T23:59:59.123456Z"}, {"2023-08-31T10:00:00Z", "2024-02-29T10:00:00Z"}, {"2024-02-29T10:00:00Z", "2024-08-29T10:00:00Z"}, {"2026-03-31T10:00:00Z", "2026-09-30T10:00:00Z"}, {"2026-01-31T10:00:00Z", "2026-07-31T10:00:00Z"}} {
		before, _ := time.Parse(time.RFC3339Nano, pair[0])
		after, _ := time.Parse(time.RFC3339Nano, pair[1])
		if !albumRetentionDeadline(before).Equal(after) {
			t.Fatal("wrong calendar deadline", pair)
		}
	}
}
