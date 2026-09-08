package platformops_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/purchase"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/internal/usage"
)

func TestObservedCostAttributionConversionAndDeletion(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.Exec(`UPDATE operations_collection SET started_at=?`, now.AddDate(0, 0, -100)); err != nil {
		t.Fatal(err)
	}
	keys := mysqlstore.NewKeyRepository(db, "health")
	ent := mysqlstore.NewEntitlementRepository(db, "health")
	u := mysqlstore.NewUsageRepository(db, "health")
	add := func(key string, freeAt time.Time) {
		t.Helper()
		if err := keys.Register(ctx, attestation.RegisteredKey{AppID: "health", KeyID: key, DeviceID: uuid.NewString(), PublicKey: []byte("fixture"), Environment: "production", Receipt: []byte("fixture")}); err != nil {
			t.Fatal(err)
		}
		r := usage.Record{RequestID: uuid.NewString(), KeyID: key, TransactionID: "free:" + key, Operation: contracts.Operation("meal_text_capture"), InputTokens: 10, OutputTokens: 2, OccurredAt: freeAt}
		for i := 0; i < 2; i++ {
			if err := u.Record(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	paidPrice, zero := int64(6000), int64(0)
	pay := func(key, original, env string, price *int64, start, purchased time.Time, revoked *time.Time) {
		t.Helper()
		if err := keys.BindTransaction(ctx, key, original); err != nil {
			t.Fatal(err)
		}
		p := purchase.Evidence{TransactionID: original + "-payment", OriginalID: original, PriceMilli: price, Currency: "CNY", PurchasedAt: purchased, StartedAt: start, SignedAt: now, RevokedAt: revoked}
		r := entitlement.Record{KeyID: key, TransactionID: original, Environment: env, StartedAt: start, ExpiresAt: now.Add(time.Hour), Payment: p}
		for i := 0; i < 2; i++ {
			if err := ent.Upsert(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	mature := now.AddDate(0, 0, -40)
	for _, key := range []string{"paid", "trial", "missing", "sandbox", "refunded", "restore", "shared"} {
		add(key, mature)
	}
	add("pending", now.AddDate(0, 0, -2))
	for _, key := range []string{"paid", "trial", "missing", "sandbox", "refunded", "restore", "shared"} {
		price := &paidPrice
		env := "production"
		start := mature.Add(time.Hour)
		var revoked *time.Time
		original := key
		switch key {
		case "trial":
			price = &zero
		case "missing":
			price = nil
		case "sandbox":
			env = "sandbox"
		case "refunded":
			revoked = &now
		case "restore":
			start = mature.AddDate(0, -1, 0)
		case "shared":
			original = "paid"
		}
		pay(key, original, env, price, start, mature.Add(2*time.Hour), revoked)
	}
	limits := costcontrol.Limits{MonthlyBudgetNanos: 100e9, MaxConcurrent: 10, LeaseDuration: time.Minute}
	controller, err := costcontrol.New(mysqlstore.NewCostControlStore(db), limits, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		key      string
		managed  bool
		expected string
	}{{"paid", false, "free"}, {"paid", true, "paid"}, {"trial", true, "subscription_zero"}, {"missing", true, "subscription_unknown"}, {"sandbox", true, "paid"}} {
		lease, err := controller.Reserve(costcontrol.WithAccess(ctx, v.key, v.managed), "health", "meal_text_capture", "ark", 10000)
		if err != nil {
			t.Fatal(err)
		}
		if err := lease.Finish(ctx, 200, true, costcontrol.Outcome{Success: true, UsageKnown: true, InputTokens: 10, OutputTokens: 2}); err != nil {
			t.Fatal(err)
		}
	}
	store := platformops.Store{DB: db}
	m, err := store.Metrics(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Insights.Segments) != 5 {
		t.Fatalf("segments: %+v", m.Insights.Segments)
	}
	for _, c := range m.Insights.Cohorts {
		if c.Observed != 8 || c.Eligible != 7 || c.Matured != 6 || c.Converted != 1 || c.Pending != 1 || c.UnknownPrice != 1 {
			t.Fatalf("cohort: %+v", c)
		}
	}
	// Current subscription state must not rewrite an already stored free attempt.
	var free, paid int
	if err := db.QueryRow(`SELECT SUM(audience='free'),SUM(audience='paid' AND environment='production') FROM ai_cost_attempts`).Scan(&free, &paid); err != nil || free != 1 || paid != 1 {
		t.Fatalf("frozen classification: %d %d %v", free, paid, err)
	}
	// A signed legacy payload lacking purchase time remains unknown instead of
	// silently becoming a free transaction; a newer verified payload can fill it.
	missingDate := purchase.Evidence{TransactionID: "legacy-date", OriginalID: "missing", PriceMilli: &paidPrice, Currency: "CNY", SignedAt: now, StartedAt: mature.Add(time.Hour)}
	if err := ent.Upsert(ctx, entitlement.Record{KeyID: "missing", TransactionID: "missing", Environment: "production", ExpiresAt: now.Add(time.Hour), StartedAt: mature.Add(time.Hour), Payment: missingDate}); err != nil {
		t.Fatal(err)
	}
	var missingTime bool
	if err := db.QueryRow(`SELECT purchased_at IS NULL FROM operations_purchase_observations WHERE transaction_id='legacy-date'`).Scan(&missingTime); err != nil || !missingTime {
		t.Fatalf("missing date lost: %v %v", missingTime, err)
	}
	missingDate.RevokedAt = &now
	missingDate.PurchasedAt = mature.Add(2 * time.Hour)
	missingDate.SignedAt = now.Add(time.Second)
	if err := ent.Upsert(ctx, entitlement.Record{KeyID: "missing", TransactionID: "missing", Environment: "production", ExpiresAt: now.Add(time.Hour), StartedAt: mature.Add(time.Hour), Payment: missingDate}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT purchased_at IS NULL FROM operations_purchase_observations WHERE transaction_id='legacy-date'`).Scan(&missingTime); err != nil || missingTime {
		t.Fatalf("new metadata did not fill date: %v %v", missingTime, err)
	}
	// A verified refund notification removes a conversion, even when restored keys exist.
	revoked := now.Add(time.Second)
	_, err = ent.ApplyNotification(ctx, entitlement.NotificationState{NotificationUUID: uuid.NewString(), OriginalTransactionID: "paid", Environment: "production", ExpiresAt: now, Payment: purchase.Evidence{TransactionID: "paid-payment", OriginalID: "paid", PriceMilli: &paidPrice, Currency: "CNY", PurchasedAt: mature.Add(2 * time.Hour), StartedAt: mature.Add(time.Hour), SignedAt: revoked, RevokedAt: &revoked}})
	if err != nil {
		t.Fatal(err)
	}
	m, err = store.Metrics(ctx, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m.Insights.Cohorts {
		if c.Converted != 0 {
			t.Fatalf("refund counted: %+v", c)
		}
	}
	if err := mysqlstore.NewPrivacyRepository(db, "health").DeletePrincipal(ctx, attestation.Principal{KeyID: "paid", TransactionID: "paid"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operations_purchase_observations WHERE original_transaction_id='paid'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("private observations retained: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM ai_cost_attempts`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("identity-free cost ledger lost: %d %v", count, err)
	}
}
