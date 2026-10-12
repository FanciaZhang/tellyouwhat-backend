package albums

import (
	"context"
	"database/sql"
	"time"
)

type reminderSubscriptionAnchor struct {
	Original, Transaction string
}

// Return the committed account revision, or zero when no longer eligible.
// Every known chain must be queried, including previously expired chains.
// This result does not replace the final reminder/registration check before APNs.
func (s *AlbumSubscriptionService) verifyRetentionReminder(ctx context.Context, r *MySQLUploads, claim albumRetentionReminder) (int64, error) {
	if s == nil || s.resolver == nil || r == nil || r.billingEnvironment != s.resolver.environment {
		return 0, ErrSubscriptionUnavailable
	}
	now := s.now().UTC()
	revision, anchors, err := r.reminderVerificationSnapshot(ctx, claim, now)
	if err != nil {
		return 0, err
	}
	budget := min(30*time.Second, claim.Until.Sub(now))
	if budget <= 0 {
		return 0, ErrLease
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	known := make(map[string]bool, len(anchors))
	for _, anchor := range anchors {
		known[anchor.Original] = true
	}
	verified := make([]VerifiedAlbumSubscription, 0, len(anchors))
	for _, anchor := range anchors {
		statuses, err := s.resolver.client.AllSubscriptionStatuses(ctx, anchor.Transaction)
		if err != nil {
			return 0, ErrSubscriptionUnavailable
		}
		for _, status := range statuses {
			transaction, err := s.resolver.verifier.VerifyTransaction(status.SignedTransaction)
			if err != nil {
				return 0, ErrSubscriptionVerification
			}
			// A newly discovered purchase belonging to this owner must be reconciled
			// before an old account snapshot can authorize an expiry reminder.
			if !known[transaction.OriginalTransactionID] && transactionBelongsToAccount(transaction, claim.Owner, r.billingEnvironment) {
				return 0, ErrSubscriptionConflict
			}
		}
		proof, err := s.resolver.resolveStatuses(claim.Owner, anchor.Original, statuses)
		if err != nil {
			return 0, err
		}
		verified = append(verified, proof)
	}
	return r.commitReminderVerification(ctx, claim, revision, anchors, verified, s.now().UTC())
}

func (r *MySQLUploads) lockLiveReminder(ctx context.Context, tx *sql.Tx, claim albumRetentionReminder, now time.Time) (albumRetentionReminder, error) {
	if !validOwner(claim.ID) || !validOwner(claim.Owner) || !validOwner(claim.Token) ||
		claim.Environment != r.billingEnvironment || (claim.Environment != "Sandbox" && claim.Environment != "Production") || now.IsZero() {
		return albumRetentionReminder{}, ErrLease
	}
	if _, err := lockAccount(ctx, tx, claim.Owner); err != nil {
		return albumRetentionReminder{}, err
	}
	current, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM album_retention_reminders WHERE reminder_id=? AND owner_id=? AND environment=? FOR UPDATE`, claim.ID, claim.Owner, claim.Environment))
	if err != nil {
		return current, err
	}
	if current.State != "pending" || current.Token != claim.Token || now.Before(current.Due) || !now.Before(current.Until) ||
		!now.Before(current.Expires) || !current.Deadline.Equal(claim.Deadline) || current.Days != claim.Days {
		return current, ErrLease
	}
	return current, nil
}

func (r *MySQLUploads) reminderVerificationSnapshot(ctx context.Context, claim albumRetentionReminder, now time.Time) (int64, []reminderSubscriptionAnchor, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	current, err := r.lockLiveReminder(ctx, tx, claim, now)
	if err != nil {
		return 0, nil, err
	}
	account, err := r.cloudAccountInTransaction(ctx, tx, claim.Owner, now)
	if err != nil {
		return 0, nil, err
	}
	if !reminderAccountEligible(account, current.Deadline) {
		return 0, nil, ErrLease
	}
	// Bound both provider work and the snapshot. Never silently truncate chains.
	rows, err := tx.QueryContext(ctx, `SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=? AND environment=? ORDER BY original_transaction_id LIMIT 129`, claim.Owner, claim.Environment)
	if err != nil {
		return 0, nil, err
	}
	var anchors []reminderSubscriptionAnchor
	for rows.Next() {
		var a reminderSubscriptionAnchor
		if err := rows.Scan(&a.Original, &a.Transaction); err != nil {
			rows.Close()
			return 0, nil, err
		}
		anchors = append(anchors, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, nil, err
	}
	if len(anchors) == 0 || len(anchors) > 128 {
		return 0, nil, ErrSubscriptionUnavailable
	}
	return account.Revision, anchors, tx.Commit()
}

func (r *MySQLUploads) commitReminderVerification(ctx context.Context, claim albumRetentionReminder, revision int64, anchors []reminderSubscriptionAnchor, proofs []VerifiedAlbumSubscription, now time.Time) (int64, error) {
	if revision < 0 || len(anchors) == 0 || len(anchors) > 128 || len(anchors) != len(proofs) {
		return 0, ErrSubscriptionVerification
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	current, err := r.lockLiveReminder(ctx, tx, claim, now)
	if err != nil {
		return 0, err
	}
	account, err := r.cloudAccountInTransaction(ctx, tx, claim.Owner, now)
	if err != nil {
		return 0, err
	}
	if account.Revision != revision {
		return 0, ErrSubscriptionConflict
	}
	for i, proof := range proofs {
		if anchors[i].Original != proof.OriginalTransactionID || proof.Environment != claim.Environment {
			return 0, ErrSubscriptionVerification
		}
		account, err = r.applySubscriptionInTransaction(ctx, tx, claim.Owner, account.Revision, proof, now)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if !reminderAccountEligible(account, current.Deadline) {
		return 0, nil
	}
	return account.Revision, nil
}
