package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type albumSubscriptionRefreshClaim struct {
	Owner, Original, Transaction, Environment, Token string
	Revision                                         int64
	Until                                            time.Time
}

func (albumSubscriptionRefreshClaim) String() string {
	return "[redacted album subscription refresh claim]"
}
func (albumSubscriptionRefreshClaim) GoString() string {
	return "[redacted album subscription refresh claim]"
}

func (r *MySQLUploads) claimSubscriptionRefresh(ctx context.Context, now time.Time) (albumSubscriptionRefreshClaim, bool, error) {
	if r.billingEnvironment != "Sandbox" && r.billingEnvironment != "Production" {
		return albumSubscriptionRefreshClaim{}, false, ErrSubscriptionUnavailable
	}
	// Read bounded candidates without holding chain locks. All mutation paths
	// then acquire the account first, avoiding account/chain lock inversion.
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id,original_transaction_id FROM album_cloud_subscriptions WHERE environment=? AND next_check_at<=? AND (refresh_until IS NULL OR refresh_until<=?) ORDER BY next_check_at,owner_id,original_transaction_id LIMIT 32`, r.billingEnvironment, now.UTC(), now.UTC())
	if err != nil {
		return albumSubscriptionRefreshClaim{}, false, err
	}
	var candidates [][2]string
	for rows.Next() {
		var candidate [2]string
		if err := rows.Scan(&candidate[0], &candidate[1]); err != nil {
			rows.Close()
			return albumSubscriptionRefreshClaim{}, false, err
		}
		candidates = append(candidates, candidate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return albumSubscriptionRefreshClaim{}, false, err
	}
	for _, candidate := range candidates {
		claim, found, err := r.claimSubscriptionCandidate(ctx, candidate[0], candidate[1], now)
		if err != nil || found {
			return claim, found, err
		}
	}
	return albumSubscriptionRefreshClaim{}, false, nil
}

func (r *MySQLUploads) claimSubscriptionCandidate(ctx context.Context, owner, original string, now time.Time) (albumSubscriptionRefreshClaim, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return albumSubscriptionRefreshClaim{}, false, err
	}
	defer tx.Rollback()
	claim := albumSubscriptionRefreshClaim{Owner: owner, Original: original, Environment: r.billingEnvironment, Token: uuid.NewString(), Until: now.UTC().Add(time.Minute)}
	err = tx.QueryRowContext(ctx, `SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=? FOR UPDATE SKIP LOCKED`, owner).Scan(&claim.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, false, nil
	}
	if err != nil {
		return claim, false, err
	}
	var due time.Time
	var until sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT transaction_id,next_check_at,refresh_until FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? AND owner_id=? FOR UPDATE SKIP LOCKED`, claim.Environment, original, owner).Scan(&claim.Transaction, &due, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, false, nil
	}
	if err != nil {
		return claim, false, err
	}
	if due.After(now) || (until.Valid && now.Before(until.Time)) {
		return claim, false, nil
	}
	if !validOwner(owner) || original == "" || claim.Transaction == "" || claim.Revision < 0 {
		return claim, false, ErrSubscriptionVerification
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_cloud_subscriptions SET refresh_token=?,refresh_until=? WHERE environment=? AND original_transaction_id=? AND owner_id=?`, claim.Token, claim.Until, claim.Environment, original, owner); err != nil {
		return claim, false, err
	}
	return claim, true, tx.Commit()
}

func (r *MySQLUploads) finishSubscriptionRefresh(ctx context.Context, c albumSubscriptionRefreshClaim, v VerifiedAlbumSubscription, now time.Time) error {
	if !validOwner(c.Owner) || !validOwner(c.Token) || c.Environment != r.billingEnvironment || c.Original != v.OriginalTransactionID || c.Environment != v.Environment {
		return ErrLease
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, c.Owner); err != nil {
		return err
	}
	var token sql.NullString
	var until sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT refresh_token,refresh_until FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? AND owner_id=? FOR UPDATE`, c.Environment, c.Original, c.Owner).Scan(&token, &until)
	if err != nil {
		return err
	}
	if !token.Valid || token.String != c.Token || !until.Valid || !now.Before(until.Time) {
		return ErrLease
	}
	if _, err := r.applySubscriptionInTransaction(ctx, tx, c.Owner, c.Revision, v, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *MySQLUploads) retrySubscriptionRefresh(ctx context.Context, c albumSubscriptionRefreshClaim, now time.Time) error {
	if !validOwner(c.Owner) || !validOwner(c.Token) || c.Environment != r.billingEnvironment {
		return ErrLease
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, c.Owner); err != nil {
		return err
	}
	var token sql.NullString
	var failures int
	err = tx.QueryRowContext(ctx, `SELECT refresh_token,refresh_failures FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? AND owner_id=? FOR UPDATE`, c.Environment, c.Original, c.Owner).Scan(&token, &failures)
	if err != nil {
		return err
	}
	if !token.Valid || token.String != c.Token {
		return ErrLease
	}
	if failures < 0 || failures > 255 {
		return ErrSubscriptionVerification
	}
	delay := time.Minute * time.Duration(1<<min(failures, 5))
	_, err = tx.ExecContext(ctx, `UPDATE album_cloud_subscriptions SET next_check_at=?,refresh_token=NULL,refresh_until=NULL,refresh_failures=? WHERE environment=? AND original_transaction_id=? AND owner_id=?`, now.UTC().Add(delay), min(failures+1, 255), c.Environment, c.Original, c.Owner)
	if err != nil {
		return err
	}
	return tx.Commit()
}
