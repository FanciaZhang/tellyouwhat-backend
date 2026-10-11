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
	Expiry, SignedAt                                   time.Time
}

const cloudSubscriptionColumns = `owner_id,original_transaction_id,transaction_id,product_id,environment,quota_bytes,active,expires_at,signed_at`

func scanCloudSubscription(row rowScanner) (cloudSubscriptionRow, error) {
	var s cloudSubscriptionRow
	err := row.Scan(&s.Owner, &s.Original, &s.Transaction, &s.Product, &s.Environment, &s.Quota, &s.Active, &s.Expiry, &s.SignedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if !validOwner(s.Owner) || s.Original == "" || s.Transaction == "" || subscriptionCapacity(s.Product) != s.Quota ||
		(s.Environment != "Sandbox" && s.Environment != "Production") || s.Expiry.IsZero() || s.SignedAt.IsZero() {
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
	if err := tx.QueryRowContext(ctx, `SELECT quota_bytes,used_bytes,reserved_bytes,subscription_revision FROM album_storage_accounts WHERE owner_id=?`, owner).
		Scan(&rawLimit, &result.UsedBytes, &result.ReservedBytes, &result.Revision); err != nil {
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
	var lastExpiry, activeExpiry time.Time
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
		} else if s.Quota == result.QuotaBytes && s.Expiry.After(activeExpiry) {
			activeExpiry = s.Expiry.UTC()
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
	if account.State != "active" {
		return time.Time{}, ErrSubscriptionRequired
	}
	if account.UsedBytes > account.QuotaBytes || account.ReservedBytes > account.QuotaBytes-account.UsedBytes {
		return time.Time{}, ErrQuota
	}
	return *account.ExpiresAt, nil
}

func (r *MySQLUploads) ApplySubscription(ctx context.Context, owner string, revision int64, v VerifiedAlbumSubscription, now time.Time) (AlbumCloudAccount, error) {
	v.ExpiresAt = v.ExpiresAt.UTC().Truncate(time.Microsecond)
	v.SignedAt = v.SignedAt.UTC().Truncate(time.Microsecond)
	if !validOwner(owner) || revision < 0 || now.IsZero() || v.OriginalTransactionID == "" || len(v.OriginalTransactionID) > 128 ||
		v.TransactionID == "" || len(v.TransactionID) > 128 || subscriptionCapacity(v.ProductID) == 0 ||
		(v.Environment != "Sandbox" && v.Environment != "Production") || (r.billingEnvironment != "" && r.billingEnvironment != v.Environment) ||
		v.ExpiresAt.Year() < 2000 || v.ExpiresAt.Year() > 9998 || v.SignedAt.UnixMilli() <= 0 || v.SignedAt.After(now.Add(5*time.Minute)) {
		return AlbumCloudAccount{}, ErrSubscriptionVerification
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	defer tx.Rollback()
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
		if old.SignedAt.After(v.SignedAt) || (old.SignedAt.Equal(v.SignedAt) && (old.Transaction != v.TransactionID || old.Product != v.ProductID || !old.Expiry.Equal(v.ExpiresAt) || old.Active != v.Active)) {
			return AlbumCloudAccount{}, ErrSubscriptionConflict
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO album_cloud_subscriptions(environment,original_transaction_id,owner_id,transaction_id,product_id,quota_bytes,active,expires_at,signed_at,verified_at)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE transaction_id=VALUES(transaction_id),product_id=VALUES(product_id),quota_bytes=VALUES(quota_bytes),active=VALUES(active),expires_at=VALUES(expires_at),signed_at=VALUES(signed_at),verified_at=VALUES(verified_at)`,
		v.Environment, v.OriginalTransactionID, owner, v.TransactionID, v.ProductID, subscriptionCapacity(v.ProductID), v.Active, v.ExpiresAt.UTC(), v.SignedAt.UTC(), now.UTC())
	if err != nil {
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
	return result, tx.Commit()
}

func (r *MySQLUploads) CheckSubscriptionSchema(ctx context.Context) error {
	for _, query := range []string{`SELECT subscription_revision FROM album_storage_accounts LIMIT 0`, `SELECT ` + cloudSubscriptionColumns + ` FROM album_cloud_subscriptions LIMIT 0`} {
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
