package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type cloudSubscriptionRow struct {
	Owner, Original, Transaction, Product, Environment string
	Quota                                              int64
	Active                                             bool
	Expiry, SignedAt, VerifiedAt                       time.Time
}

const cloudSubscriptionColumns = `owner_id,original_transaction_id,transaction_id,product_id,environment,quota_bytes,active,expires_at,signed_at,verified_at`

func scanCloudSubscription(row rowScanner) (cloudSubscriptionRow, error) {
	var s cloudSubscriptionRow
	err := row.Scan(&s.Owner, &s.Original, &s.Transaction, &s.Product, &s.Environment, &s.Quota, &s.Active, &s.Expiry, &s.SignedAt, &s.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if !validOwner(s.Owner) || s.Original == "" || s.Transaction == "" || subscriptionCapacity(s.Product) != s.Quota ||
		(s.Environment != "Sandbox" && s.Environment != "Production") || s.Expiry.IsZero() || s.SignedAt.IsZero() || s.VerifiedAt.IsZero() {
		return s, ErrSubscriptionVerification
	}
	return s, nil
}

func (r *MySQLUploads) cloudAccountInTransaction(ctx context.Context, tx *sql.Tx, owner string, now time.Time) (AlbumCloudAccount, error) {
	result := AlbumCloudAccount{AccountID: owner, State: "none"}
	if !validOwner(owner) || now.IsZero() {
		return result, ErrOwner
	}
	var rawLimit int64
	if err := tx.QueryRowContext(ctx, `SELECT s.quota_bytes,s.used_bytes,s.reserved_bytes,s.subscription_revision,COALESCE(a.storekit_account_token,s.owner_id) FROM album_storage_accounts s LEFT JOIN album_accounts a ON a.account_id=s.owner_id WHERE s.owner_id=?`, owner).
		Scan(&rawLimit, &result.UsedBytes, &result.ReservedBytes, &result.Revision, &result.AppAccountToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrOwner
		}
		return result, err
	}
	query := `SELECT ` + cloudSubscriptionColumns + ` FROM album_cloud_subscriptions WHERE owner_id=?`
	args := []any{owner}
	if r.billingEnvironment != "" {
		query += ` AND environment=?`
		args = append(args, r.billingEnvironment)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var lastExpiry, activeExpiry, freshExpiry time.Time
	for rows.Next() {
		s, err := scanCloudSubscription(rows)
		if err != nil {
			return result, err
		}
		if s.Expiry.After(lastExpiry) {
			lastExpiry = s.Expiry.UTC()
		}
		if !s.Active || !now.Before(s.Expiry) {
			continue
		}
		if s.Quota > result.QuotaBytes {
			result.QuotaBytes, result.ProductID, activeExpiry = s.Quota, s.Product, s.Expiry.UTC()
			freshExpiry = minTime(s.Expiry.UTC(), s.VerifiedAt.UTC().Add(albumSubscriptionFreshness))
		} else if s.Quota == result.QuotaBytes {
			if s.Expiry.After(activeExpiry) {
				activeExpiry = s.Expiry.UTC()
			}
			fresh := minTime(s.Expiry.UTC(), s.VerifiedAt.UTC().Add(albumSubscriptionFreshness))
			if fresh.After(freshExpiry) {
				freshExpiry = fresh
			}
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if !lastExpiry.IsZero() {
		deadline := albumRetentionDeadline(lastExpiry)
		result.RetentionDeadline = &deadline
		result.ExpiresAt = &lastExpiry
		result.State = "expired"
		if !now.Before(deadline) {
			result.State = "retention_due"
		}
	}
	if result.QuotaBytes > 0 {
		result.State, result.ExpiresAt = "active", &activeExpiry
		if r.billingEnvironment != "" {
			result.VerificationExpiresAt = &freshExpiry
			result.VerificationPending = !now.Before(freshExpiry)
		}
	} else if r.billingEnvironment == "" && lastExpiry.IsZero() {
		// Non-gateway repository users explicitly provision protocol-test accounts.
		result.QuotaBytes = rawLimit
	}
	if result.UsedBytes < 0 || result.ReservedBytes < 0 || result.Revision < 0 {
		return result, ErrSubscriptionVerification
	}
	if result.UsedBytes <= result.QuotaBytes && result.ReservedBytes <= result.QuotaBytes-result.UsedBytes {
		result.AvailableBytes = result.QuotaBytes - result.UsedBytes - result.ReservedBytes
	}
	return result, nil
}

func (r *MySQLUploads) CloudAccount(ctx context.Context, owner string, now time.Time) (AlbumCloudAccount, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	defer tx.Rollback()
	result, err := r.cloudAccountInTransaction(ctx, tx, owner, now)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (r *MySQLUploads) AllowNewCloudMedia(ctx context.Context, owner string, now time.Time) error {
	_, err := r.CloudMediaGrantDeadline(ctx, owner, now)
	return err
}

func (r *MySQLUploads) CloudMediaGrantDeadline(ctx context.Context, owner string, now time.Time) (time.Time, error) {
	if r.billingEnvironment == "" {
		return time.Time{}, nil
	}
	if r.billingEnvironment != "Sandbox" && r.billingEnvironment != "Production" {
		return time.Time{}, ErrSubscriptionRequired
	}
	account, err := r.CloudAccount(ctx, owner, now)
	if err != nil {
		return time.Time{}, err
	}
	return cloudAccountMediaDeadline(account)
}

func cloudAccountMediaDeadline(account AlbumCloudAccount) (time.Time, error) {
	if account.State != "active" {
		return time.Time{}, ErrSubscriptionRequired
	}
	if account.UsedBytes > account.QuotaBytes || account.ReservedBytes > account.QuotaBytes-account.UsedBytes {
		return time.Time{}, ErrQuota
	}
	if account.VerificationPending || account.VerificationExpiresAt == nil {
		return time.Time{}, ErrSubscriptionStale
	}
	return minTime(*account.ExpiresAt, *account.VerificationExpiresAt), nil
}

func (r *MySQLUploads) ApplySubscription(ctx context.Context, owner string, revision int64, v VerifiedAlbumSubscription, now time.Time) (AlbumCloudAccount, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	defer tx.Rollback()
	if err := requireMediaAccount(ctx, tx, owner); err != nil {
		return AlbumCloudAccount{}, err
	}
	result, err := r.applySubscriptionInTransaction(ctx, tx, owner, revision, v, now)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (r *MySQLUploads) applySubscriptionInTransaction(ctx context.Context, tx *sql.Tx, owner string, revision int64, v VerifiedAlbumSubscription, now time.Time) (AlbumCloudAccount, error) {
	v.ExpiresAt = v.ExpiresAt.UTC().Truncate(time.Microsecond)
	v.SignedAt = v.SignedAt.UTC().Truncate(time.Microsecond)
	if !validOwner(owner) || revision < 0 || now.IsZero() || v.OriginalTransactionID == "" || len(v.OriginalTransactionID) > 128 ||
		v.TransactionID == "" || len(v.TransactionID) > 128 || subscriptionCapacity(v.ProductID) == 0 ||
		(v.Environment != "Sandbox" && v.Environment != "Production") || (r.billingEnvironment != "" && r.billingEnvironment != v.Environment) ||
		v.ExpiresAt.Year() < 2000 || v.ExpiresAt.Year() > 9998 || v.SignedAt.UnixMilli() <= 0 || v.SignedAt.After(now.Add(5*time.Minute)) {
		return AlbumCloudAccount{}, ErrSubscriptionVerification
	}
	if _, err := r.lockAccount(ctx, tx, owner); err != nil {
		return AlbumCloudAccount{}, err
	}
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&currentRevision); err != nil {
		return AlbumCloudAccount{}, err
	}
	if currentRevision != revision {
		return AlbumCloudAccount{}, ErrSubscriptionConflict
	}
	// The immutable original-chain key also serializes competing account claims.
	old, err := scanCloudSubscription(tx.QueryRowContext(ctx, `SELECT `+cloudSubscriptionColumns+` FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? FOR UPDATE`, v.Environment, v.OriginalTransactionID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return AlbumCloudAccount{}, err
	}
	if err == nil {
		if old.Owner != owner {
			return AlbumCloudAccount{}, ErrSubscriptionBinding
		}
		// The same signed transaction can move from active to expired solely
		// because its verified expiry has passed. This does not permit an equal
		// or older proof to change the transaction, capacity or expiry, nor to
		// reactivate a previously inactive chain.
		sameTerms := old.Transaction == v.TransactionID && old.Product == v.ProductID && old.Expiry.Equal(v.ExpiresAt)
		clockExpiry := sameTerms && old.Active && !v.Active && !now.Before(v.ExpiresAt)
		if old.SignedAt.After(v.SignedAt) || (old.SignedAt.Equal(v.SignedAt) && (!sameTerms || (old.Active != v.Active && !clockExpiry))) {
			return AlbumCloudAccount{}, ErrSubscriptionConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_cloud_subscriptions(environment,original_transaction_id,owner_id,transaction_id,product_id,quota_bytes,active,expires_at,signed_at,verified_at)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE transaction_id=VALUES(transaction_id),product_id=VALUES(product_id),quota_bytes=VALUES(quota_bytes),active=VALUES(active),expires_at=VALUES(expires_at),signed_at=VALUES(signed_at),verified_at=VALUES(verified_at)`,
		v.Environment, v.OriginalTransactionID, owner, v.TransactionID, v.ProductID, subscriptionCapacity(v.ProductID), v.Active, v.ExpiresAt.UTC(), v.SignedAt.UTC(), now.UTC())
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	interval := albumSubscriptionRefreshInterval
	if !v.Active {
		interval = 24 * time.Hour
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_cloud_subscriptions SET next_check_at=?,refresh_token=NULL,refresh_until=NULL,refresh_failures=0 WHERE environment=? AND original_transaction_id=? AND owner_id=?`, now.UTC().Add(interval), v.Environment, v.OriginalTransactionID, owner); err != nil {
		return AlbumCloudAccount{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET subscription_revision=subscription_revision+1 WHERE owner_id=?`, owner); err != nil {
		return AlbumCloudAccount{}, err
	}
	result, err := r.cloudAccountInTransaction(ctx, tx, owner, now)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET quota_bytes=? WHERE owner_id=?`, result.QuotaBytes, owner); err != nil {
		return result, err
	}
	if err := r.syncRetentionReminders(ctx, tx, result, now); err != nil {
		return AlbumCloudAccount{}, err
	}
	if err := r.cancelRenewedRetentionPurges(ctx, tx, result); err != nil {
		return AlbumCloudAccount{}, err
	}
	return result, nil
}

func (r *MySQLUploads) CheckSubscriptionSchema(ctx context.Context) error {
	for _, query := range []string{`SELECT storekit_account_token FROM album_accounts LIMIT 0`, `SELECT subscription_revision FROM album_storage_accounts LIMIT 0`, `SELECT ` + objectPurgeColumns + ` FROM album_object_purges LIMIT 0`, `SELECT ` + reminderColumns + `,delivered_at FROM album_retention_reminders LIMIT 0`, `SELECT ` + cloudSubscriptionColumns + `,next_check_at,refresh_token,refresh_until,refresh_failures FROM album_cloud_subscriptions LIMIT 0`, `SELECT environment,notification_id,payload_digest,owner_id,original_transaction_id,signed_at,processed_at FROM album_subscription_notifications LIMIT 0`} {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// The caller supplies a token from a verified Apple JWS, never a client owner ID.
// Legacy purchases use their original account UUID until explicitly migrated.
func (r *MySQLUploads) SubscriptionOwner(ctx context.Context, token string) (string, error) {
	if !validOwner(token) {
		return "", ErrSubscriptionBinding
	}
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id FROM album_storage_accounts WHERE owner_id=? UNION SELECT s.owner_id FROM album_accounts a JOIN album_storage_accounts s ON s.owner_id=a.account_id WHERE a.storekit_account_token=? LIMIT 2`, token, token)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var owner string
	for rows.Next() {
		if owner != "" {
			return "", ErrSubscriptionBinding
		}
		if err = rows.Scan(&owner); err != nil {
			return "", err
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if !validOwner(owner) {
		return "", ErrSubscriptionBinding
	}
	return owner, nil
}
