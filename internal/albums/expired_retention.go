package albums

import (
	"context"
	"database/sql"
	"time"
)

// This capability is internal and short lived; a cached CloudAccount is not
// authority to delete. Every consumption rechecks the account revision in SQL.
type expiredRetentionProof struct {
	owner, environment string
	revision           int64
	deadline, until    time.Time
}

func retentionDue(a AlbumCloudAccount, deadline time.Time) bool {
	return a.State == "retention_due" && a.RetentionDeadline != nil && a.RetentionDeadline.Equal(deadline) && (a.UsedBytes > 0 || a.ReservedBytes > 0)
}

func (s *AlbumSubscriptionService) verifyExpiredRetention(ctx context.Context, r *MySQLUploads, owner string) (expiredRetentionProof, error) {
	if s == nil || s.resolver == nil || r == nil || !validOwner(owner) || r.billingEnvironment != s.resolver.environment {
		return expiredRetentionProof{}, ErrSubscriptionUnavailable
	}
	start := s.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	defer tx.Rollback()
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		return expiredRetentionProof{}, err
	}
	a, err := r.cloudAccountInTransaction(ctx, tx, owner, start)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	if a.RetentionDeadline == nil || !retentionDue(a, *a.RetentionDeadline) {
		return expiredRetentionProof{}, ErrLease
	}
	rows, err := tx.QueryContext(ctx, `SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=? AND environment=? ORDER BY original_transaction_id LIMIT 129`, owner, r.billingEnvironment)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	var anchors []reminderSubscriptionAnchor
	for rows.Next() {
		var item reminderSubscriptionAnchor
		if err := rows.Scan(&item.Original, &item.Transaction); err != nil {
			rows.Close()
			return expiredRetentionProof{}, err
		}
		anchors = append(anchors, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return expiredRetentionProof{}, err
	}
	if len(anchors) == 0 || len(anchors) > 128 {
		return expiredRetentionProof{}, ErrSubscriptionUnavailable
	}
	if err := tx.Commit(); err != nil {
		return expiredRetentionProof{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proofs, err := s.resolveKnownSubscriptions(ctx, owner, anchors)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	tx, err = r.db.BeginTx(ctx, nil)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	defer tx.Rollback()
	if _, err = lockAccount(ctx, tx, owner); err != nil {
		return expiredRetentionProof{}, err
	}
	now := s.now().UTC()
	current, err := r.cloudAccountInTransaction(ctx, tx, owner, now)
	if err != nil {
		return expiredRetentionProof{}, err
	}
	if current.Revision != a.Revision {
		return expiredRetentionProof{}, ErrSubscriptionConflict
	}
	for _, proof := range proofs {
		current, err = r.applySubscriptionInTransaction(ctx, tx, owner, current.Revision, proof, now)
		if err != nil {
			return expiredRetentionProof{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return expiredRetentionProof{}, err
	}
	until := start.Add(30 * time.Second).Truncate(time.Microsecond)
	if !retentionDue(current, *a.RetentionDeadline) || !s.now().UTC().Before(until) {
		return expiredRetentionProof{}, ErrLease
	}
	return expiredRetentionProof{owner, r.billingEnvironment, current.Revision, *a.RetentionDeadline, until}, nil
}

func (r *MySQLUploads) checkExpiredRetention(ctx context.Context, tx *sql.Tx, proof expiredRetentionProof, now time.Time) error {
	if !validOwner(proof.owner) || proof.environment != r.billingEnvironment || (proof.environment != "Sandbox" && proof.environment != "Production") || proof.revision <= 0 || now.IsZero() || proof.deadline.After(now) || !now.Before(proof.until) {
		return ErrLease
	}
	a, err := r.cloudAccountInTransaction(ctx, tx, proof.owner, now)
	if err != nil {
		return err
	}
	if a.Revision != proof.revision || !retentionDue(a, proof.deadline) {
		return ErrLease
	}
	return nil
}

// Subscription updates and first-delete authorization share the owner lock.
// Removing unstarted plans also invalidates leases and stale provider inventories.
func (r *MySQLUploads) cancelRenewedRetentionPurges(ctx context.Context, tx *sql.Tx, a AlbumCloudAccount) error {
	if r.billingEnvironment == "" {
		return nil
	}
	if a.RetentionDeadline != nil && retentionDue(a, *a.RetentionDeadline) {
		_, err := tx.ExecContext(ctx, `DELETE FROM album_object_purges WHERE owner_id=? AND environment=? AND reason='retention' AND started_at IS NULL AND state<>'completed' AND trash_expires_at<>?`, a.AccountID, r.billingEnvironment, *a.RetentionDeadline)
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM album_object_purges WHERE owner_id=? AND environment=? AND reason='retention' AND started_at IS NULL AND state<>'completed'`, a.AccountID, r.billingEnvironment)
	return err
}
