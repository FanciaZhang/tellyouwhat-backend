package adminportal

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/testutil"
)

func TestOfferMetricsCountCodeSubscriptionsWithoutRenewalOrPromotionInflation(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	for _, v := range []struct {
		app, env, tx, original string
		kind                   int
	}{
		{"health", "production", "first", "subscriber-a", 3},
		{"health", "production", "renewal", "subscriber-a", 3},
		{"health", "production", "second", "subscriber-b", 3},
		{"health", "production", "promotion", "subscriber-c", 2},
		{"health", "sandbox", "sandbox", "test-a", 3},
		{"journal", "production", "other-app", "subscriber-d", 3},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,offer_type,redeemed_at,expires_at) VALUES(?,?,UNHEX(SHA2(?,256)),UNHEX(SHA2(?,256)),'FRIENDS',?,?,?)`, v.app, v.env, v.tx, v.original, v.kind, time.Now(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := NewMySQLMetricsReader(db).OfferMetrics(ctx, "health")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d environments", len(rows))
	}
	for _, r := range rows {
		want := 2
		if r.Environment == "sandbox" {
			want = 1
		}
		if r.OfferIdentifier != "FRIENDS" || r.Redemptions != want || r.UniqueAccounts != want {
			t.Fatalf("incorrect counts: %+v", r)
		}
	}
}

func TestOfferMetricsKeepProductsAndLegacySeparate(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	for _, product := range []string{"app.monthly", "app.annual", ""} {
		if _, err := db.Exec(`INSERT INTO app_store_offer_redemptions(app_id,environment,transaction_hash,original_transaction_hash,offer_identifier,product_id,offer_type,redeemed_at,expires_at) VALUES('health','production',UNHEX(SHA2(?,256)),UNHEX(SHA2(?,256)),'FRIENDS',?,3,?,?)`, product, product, product, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := NewMySQLMetricsReader(db).OfferMetrics(ctx, "health")
	if err != nil || len(rows) != 3 {
		t.Fatalf("product/legacy grouping %+v %v", rows, err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ProductID] || r.Redemptions != 1 {
			t.Fatalf("duplicated evidence %+v", r)
		}
		seen[r.ProductID] = true
	}
}
