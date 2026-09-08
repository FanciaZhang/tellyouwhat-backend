package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/purchase"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

// Only successful free business requests enter the observed cohort. Existing
// subscriptions are excluded at reporting time using Apple's original purchase date.
func observeFree(ctx context.Context, tx *sql.Tx, app string, r usage.Record) error {
	if !strings.HasPrefix(r.TransactionID, quota.FreeRecognitionTransactionPrefix) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operations_free_cohorts(app_id,key_id,first_free_at)
 SELECT ?,?,? FROM app_attest_keys k JOIN operations_collection c ON c.singleton_id=1
 WHERE k.app_id=? AND k.key_id=? AND k.environment='production' AND ?>=c.started_at
 ON DUPLICATE KEY UPDATE first_free_at=LEAST(first_free_at,VALUES(first_free_at))`, app, r.KeyID, r.OccurredAt, app, r.KeyID, r.OccurredAt)
	return err
}

// One signed transaction may be restored on multiple keys. Reporting deduplicates
// subscription acquisitions across those keys; deletion cascades with the keys.
func observePurchase(ctx context.Context, tx *sql.Tx, app, key, environment string, e purchase.Evidence) error {
	environment = strings.ToLower(environment)
	if e.TransactionID == "" || e.OriginalID == "" || e.PurchasedAt.IsZero() || e.SignedAt.IsZero() || (environment != "production" && environment != "sandbox") {
		return nil
	}
	var price any
	if e.HasPrice() {
		price = *e.PriceMilli
	}
	currency := e.Currency
	if len(currency) != 3 {
		currency = ""
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operations_purchase_observations
 (app_id,key_id,transaction_id,original_transaction_id,environment,price_milli,currency,purchased_at,started_at,signed_at,revoked_at,observed_at)
 SELECT k.app_id,k.key_id,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6) FROM app_attest_keys k
 WHERE k.app_id=? AND ((?<>'' AND k.key_id=?) OR (?='' AND k.transaction_id=?))
 ON DUPLICATE KEY UPDATE
 price_milli=IF(VALUES(signed_at)>=signed_at,VALUES(price_milli),price_milli),
 currency=IF(VALUES(signed_at)>=signed_at,VALUES(currency),currency),
 revoked_at=IF(VALUES(signed_at)>=signed_at,VALUES(revoked_at),revoked_at),
 signed_at=GREATEST(signed_at,VALUES(signed_at))`, e.TransactionID, e.OriginalID, environment, price, currency, e.PurchasedAt.UTC(), nullableVoiceDate(e.StartedAt), e.SignedAt.UTC(), e.RevokedAt, app, key, key, key, e.OriginalID)
	return err
}

func costAudience(ctx context.Context, tx *sql.Tx, app string, now time.Time) (string, string, error) {
	access, ok := costcontrol.AccessFrom(ctx)
	if !ok {
		return "unknown", "unknown", nil
	}
	var keyEnv string
	err := tx.QueryRowContext(ctx, `SELECT environment FROM app_attest_keys WHERE app_id=? AND key_id=?`, app, access.KeyID).Scan(&keyEnv)
	if errors.Is(err, sql.ErrNoRows) {
		return "unknown", "unknown", nil
	}
	if err != nil {
		return "", "", err
	}
	if !access.Managed {
		return "free", keyEnv, nil
	}
	var env string
	var price sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT environment,price_milli FROM managed_entitlements WHERE app_id=? AND key_id=? AND expires_at>?`, app, access.KeyID, now).Scan(&env, &price)
	if errors.Is(err, sql.ErrNoRows) {
		return "subscription_unknown", "unknown", nil
	}
	if err != nil {
		return "", "", err
	}
	if !price.Valid {
		return "subscription_unknown", env, nil
	}
	if price.Int64 > 0 {
		return "paid", env, nil
	}
	return "subscription_zero", env, nil
}

func purchasePrice(e purchase.Evidence) any {
	if e.HasPrice() {
		return *e.PriceMilli
	}
	return nil
}
