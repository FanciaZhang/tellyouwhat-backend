package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProductionCloudRightsAreUnaffectedBySandboxLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, paidState := range []string{"active", "expired", "retention_due"} {
		t.Run(paidState, func(t *testing.T) {
			paid := cloudSubscriptionRow{Environment: "Production", Product: Cloud50GBAnnualProduct, Quota: 50_000_000_000, Active: true, Expiry: now.Add(time.Hour), VerifiedAt: now}
			if paidState != "active" {
				paid.Active, paid.Expiry = false, now.Add(-time.Hour)
				if paidState == "retention_due" {
					paid.Expiry = now.AddDate(0, -7, 0)
				}
			}
			var production cloudSubscriptionSummary
			production.add(paid, now)
			baseline := AlbumCloudAccount{State: "none"}
			applyProductionCloudEntitlements(&baseline, production, cloudSubscriptionSummary{}, now, 1_000_000_000)
			if baseline.State != paidState || baseline.QuotaKind != "subscription" {
				t.Fatal("wrong paid fixture state", baseline.State)
			}
			for _, expiry := range []time.Time{now.Add(time.Minute), now.Add(-time.Minute), now.AddDate(0, -12, 0)} {
				var sandbox cloudSubscriptionSummary
				sandbox.add(cloudSubscriptionRow{Environment: "Sandbox", Product: Cloud500GBAnnualProduct, Quota: 500_000_000_000, Active: true, Expiry: expiry, VerifiedAt: now}, now)
				got := AlbumCloudAccount{State: "none"}
				applyProductionCloudEntitlements(&got, production, sandbox, now, 1_000_000_000)
				if !reflect.DeepEqual(baseline, got) {
					t.Fatal("Sandbox changed paid rights, verification or retention")
				}
			}
		})
	}
}

func TestSandboxCapacityIsCappedAndExpiryNeverProducesDeletionDeadline(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, elapsed := range []time.Duration{-time.Minute, time.Minute, 400 * 24 * time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			var sandbox cloudSubscriptionSummary
			sandbox.add(cloudSubscriptionRow{Environment: "Sandbox", Product: Cloud500GBAnnualProduct, Quota: 500_000_000_000, Active: true, Expiry: now.Add(-elapsed), VerifiedAt: now}, now)
			a := AlbumCloudAccount{State: "none", UsedBytes: 500_000_000}
			applyProductionCloudEntitlements(&a, cloudSubscriptionSummary{}, sandbox, now, 1_000_000_000)
			if a.QuotaKind != "sandbox" || a.RetentionDeadline != nil || a.State == "retention_due" || retentionDue(a, now) {
				t.Fatal("Sandbox expiry authorized retention cleanup")
			}
			if elapsed < 0 {
				if a.State != "active" || a.QuotaBytes != 1_000_000_000 {
					t.Fatal("Sandbox exceeded configured capacity")
				}
			} else if a.State != "expired" || a.QuotaBytes != 0 {
				t.Fatal("expired Sandbox still allows new uploads")
			}
		})
	}
	// Reject a forged or stale Sandbox cleanup capability before opening storage.
	r := &MySQLUploads{billingEnvironment: "Sandbox", productionSandboxQuota: 1_000_000_000}
	if err := r.checkExpiredRetention(context.Background(), nil, expiredRetentionProof{environment: "Sandbox"}, now); !errors.Is(err, ErrLease) {
		t.Fatal("Sandbox cleanup capability was accepted")
	}
}

type dualCommerceRepository struct {
	account       AlbumCloudAccount
	applied       []VerifiedAlbumSubscription
	notifications []string
}

func (r *dualCommerceRepository) CloudAccount(context.Context, string, time.Time) (AlbumCloudAccount, error) {
	return r.account, nil
}
func (r *dualCommerceRepository) SubscriptionOwner(context.Context, string) (string, error) {
	return r.account.AccountID, nil
}
func (r *dualCommerceRepository) ApplySubscription(_ context.Context, _ string, _ int64, v VerifiedAlbumSubscription, _ time.Time) (AlbumCloudAccount, error) {
	r.applied = append(r.applied, v)
	return r.account, nil
}
func (r *dualCommerceRepository) NotificationProcessed(context.Context, albumNotificationReceipt) (bool, error) {
	return false, nil
}
func (r *dualCommerceRepository) ApplySubscriptionNotification(_ context.Context, n albumNotificationReceipt, _ int64, v VerifiedAlbumSubscription, _ time.Time) error {
	r.notifications = append(r.notifications, n.Environment)
	r.applied = append(r.applied, v)
	return nil
}
func (r *dualCommerceRepository) RecordSubscriptionTestNotification(_ context.Context, n albumNotificationReceipt, _ time.Time) error {
	r.notifications = append(r.notifications, n.Environment)
	return nil
}

func TestProductionCommerceRoutesOnlyVerifiedTransactionsAndNotifications(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	owner := uuid.NewString()
	config := &AlbumSubscriptionConfig{BundleID: albumAppleAudience, Environment: "Both", AppAppleID: 12345,
		SandboxQuotaBytes: 1_000_000_000, IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: fixture.roots}
	signed := map[string]string{}
	for _, environment := range []string{"Production", "Sandbox"} {
		payload := fixture.payload(owner)
		payload["bundleId"], payload["environment"] = albumAppleAudience, environment
		signed[environment] = fixture.sign(t, payload)
	}
	calls := map[string]int{}
	providerFailure := false
	transport := appleTestTransport(func(req *http.Request) (*http.Response, error) {
		environment := "Production"
		if req.URL.Host == "api.storekit-sandbox.apple.com" {
			environment = "Sandbox"
		} else if req.URL.Host != "api.storekit.apple.com" {
			t.Fatal("untrusted App Store endpoint")
		}
		calls[environment]++
		if providerFailure {
			return nil, errors.New("synthetic provider unavailable")
		}
		data, _ := json.Marshal(map[string]any{"bundleId": albumAppleAudience, "appAppleId": 12345, "environment": environment,
			"data": []any{map[string]any{"lastTransactions": []any{map[string]any{"status": 1, "signedTransactionInfo": signed[environment]}}}}})
		return appleResponse(string(data)), nil
	})
	repository := NewMySQLUploads(nil).WithSubscriptionEnvironment("Production")
	service, err := newConfiguredAlbumSubscriptions(repository, config, transport)
	if err != nil || service.sandbox == nil || repository.productionSandboxQuota != config.SandboxQuotaBytes {
		t.Fatal("dual commerce assembly failed", err)
	}
	prodRepo, testRepo := &dualCommerceRepository{account: AlbumCloudAccount{AccountID: owner, AppAccountToken: owner}}, &dualCommerceRepository{account: AlbumCloudAccount{AccountID: owner, AppAccountToken: owner}}
	service.repository, service.sandbox.repository = prodRepo, testRepo
	for _, environment := range []string{"Production", "Sandbox"} {
		if _, err := service.Sync(context.Background(), owner, signed[environment]); err != nil {
			t.Fatal("verified purchase rejected", environment, err)
		}
		for _, kind := range []string{"TEST", "DID_RENEW"} {
			data := map[string]any{"bundleId": albumAppleAudience, "appAppleId": 12345, "environment": environment}
			if kind != "TEST" {
				data["signedTransactionInfo"] = signed[environment]
				data["status"] = 1
			}
			payload := map[string]any{"notificationUUID": uuid.NewString(), "notificationType": kind, "signedDate": now.UnixMilli(), "version": "2.0", "data": data}
			if err := service.receiveNotification(context.Background(), fixture.sign(t, payload)); err != nil {
				t.Fatal("verified notification rejected", environment, kind, err)
			}
		}
	}
	if len(prodRepo.applied) != 2 || len(testRepo.applied) != 2 || !reflect.DeepEqual(prodRepo.notifications, []string{"Production", "Production"}) || !reflect.DeepEqual(testRepo.notifications, []string{"Sandbox", "Sandbox"}) {
		t.Fatal("purchase or notification crossed storage scopes")
	}
	for _, row := range prodRepo.applied {
		if row.Environment != "Production" {
			t.Fatal("Sandbox wrote production scope")
		}
	}
	for _, row := range testRepo.applied {
		if row.Environment != "Sandbox" {
			t.Fatal("production wrote Sandbox scope")
		}
	}
	before := calls["Sandbox"]
	providerFailure = true
	if _, err := service.Sync(context.Background(), owner, signed["Production"]); err == nil || calls["Sandbox"] != before {
		t.Fatal("production provider failure fell back to Sandbox")
	}
	allBefore := calls["Production"] + calls["Sandbox"]
	if _, err := service.Sync(context.Background(), owner, "not-a-JWS"); err == nil || calls["Production"]+calls["Sandbox"] != allBefore {
		t.Fatal("unverified client data selected a provider")
	}
}
