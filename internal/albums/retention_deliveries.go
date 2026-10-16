package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

type albumRetentionDelivery struct {
	ID           string
	Registration albumPushRegistration
	Environment  string
}

const reminderTargetsSQL = ` FROM album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id JOIN album_accounts a ON a.account_id=s.account_id JOIN album_apple_grants g ON g.grant_id=s.grant_id AND g.account_id=a.account_id
 LEFT JOIN album_retention_deliveries d ON d.reminder_id=? AND d.registration_id=p.registration_id AND d.registration_revision=p.revision
 WHERE a.account_id=? AND p.environment IN (?,?) AND p.state='enabled' AND s.revoked_at IS NULL AND a.state='active' AND g.revoked_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM album_retention_deliveries accepted JOIN album_push_registrations sent ON sent.registration_id=accepted.registration_id WHERE accepted.reminder_id=? AND sent.session_id=p.session_id AND accepted.state='accepted')
 AND (d.state IS NULL OR d.state='pending')`

// All provider calls happen after releasing database locks. The exact revision
// returned by the all-chain verification fences concurrent entitlement changes.
func pushEnvironmentPair(environments []string) (string, string, error) {
	if len(environments) < 1 || len(environments) > 2 {
		return "", "", ErrLease
	}
	for _, value := range environments {
		if value != "development" && value != "production" {
			return "", "", ErrLease
		}
	}
	return environments[0], environments[len(environments)-1], nil
}

func (r *MySQLUploads) nextRetentionDelivery(ctx context.Context, claim albumRetentionReminder, revision int64, environments []string, now time.Time) (albumRetentionDelivery, bool, error) {
	var delivery albumRetentionDelivery
	first, second, err := pushEnvironmentPair(environments)
	if revision < 1 || err != nil {
		return delivery, false, ErrLease
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return delivery, false, err
	}
	defer tx.Rollback()
	if _, err = r.lockLiveReminder(ctx, tx, claim, now); err != nil {
		return delivery, false, err
	}
	account, err := r.cloudAccountInTransaction(ctx, tx, claim.Owner, now)
	if err != nil {
		return delivery, false, err
	}
	if account.Revision != revision || !reminderAccountEligible(account, claim.Deadline) {
		return delivery, false, ErrLease
	}
	err = tx.QueryRowContext(ctx, `SELECT p.registration_id,p.revision,p.environment`+reminderTargetsSQL+` AND (d.available_at IS NULL OR d.available_at<=?) ORDER BY p.registration_id LIMIT 1`, claim.ID, claim.Owner, first, second, claim.ID, now).Scan(&delivery.Registration.ID, &delivery.Registration.Revision, &delivery.Environment)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery, false, tx.Commit()
	}
	if err != nil {
		return delivery, false, err
	}
	delivery.Registration.State = "enabled"
	if _, err = tx.ExecContext(ctx, `INSERT INTO album_retention_deliveries(delivery_id,reminder_id,registration_id,registration_revision,available_at,lease_token) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE lease_token=VALUES(lease_token)`, uuid.NewString(), claim.ID, delivery.Registration.ID, delivery.Registration.Revision, now, claim.Token); err != nil {
		return delivery, false, err
	}
	var state string
	var available time.Time
	err = tx.QueryRowContext(ctx, `SELECT delivery_id,state,available_at FROM album_retention_deliveries WHERE reminder_id=? AND registration_id=? AND registration_revision=? FOR UPDATE`, claim.ID, delivery.Registration.ID, delivery.Registration.Revision).Scan(&delivery.ID, &state, &available)
	if err != nil {
		return delivery, false, err
	}
	// A late response may have committed after the candidate snapshot was read.
	if state != "pending" || available.After(now) {
		return delivery, false, tx.Commit()
	}
	return delivery, true, tx.Commit()
}

// Persist late acceptance even after the parent lease expires, unless a newer
// attempt has already taken over. Never reopen a cancelled reminder.
func (r *MySQLUploads) recordRetentionDelivery(ctx context.Context, claim albumRetentionReminder, delivery albumRetentionDelivery, result albumAPNSResult, now time.Time) error {
	if !validOwner(claim.ID) || !validOwner(claim.Token) || !validOwner(delivery.ID) || !validOwner(delivery.Registration.ID) || delivery.Registration.Revision < 1 || now.IsZero() {
		return ErrLease
	}
	state, available := "pending", now.Add(15*time.Minute)
	var accepted any
	switch result.Status {
	case "accepted":
		state, accepted = "accepted", now
	case "unregistered", "rejected":
		state = "rejected"
	case "retry":
		if result.RetryAt.After(available) {
			available = result.RetryAt
		}
	case "": // An ambiguous network outcome retries with the same APNs request ID.
	default:
		return errAlbumAPNS
	}
	response, err := r.db.ExecContext(ctx, `UPDATE album_retention_deliveries SET state=?,available_at=?,accepted_at=? WHERE delivery_id=? AND reminder_id=? AND registration_id=? AND registration_revision=? AND lease_token=? AND state='pending'`, state, available, accepted, delivery.ID, claim.ID, delivery.Registration.ID, delivery.Registration.Revision, claim.Token)
	if err != nil {
		return err
	}
	count, err := response.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLease
	}
	return nil
}

func (r *MySQLUploads) settleRetentionDispatch(ctx context.Context, claim albumRetentionReminder, environments []string, now time.Time) error {
	first, second, err := pushEnvironmentPair(environments)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = r.lockLiveReminder(ctx, tx, claim, now); err != nil {
		return err
	}
	account, err := r.cloudAccountInTransaction(ctx, tx, claim.Owner, now)
	if err != nil {
		return err
	}
	if !reminderAccountEligible(account, claim.Deadline) {
		return ErrLease
	}
	var pending, accepted int
	var available sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),MIN(COALESCE(d.available_at,?))`+reminderTargetsSQL, now, claim.ID, claim.Owner, first, second, claim.ID).Scan(&pending, &available)
	if err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_retention_deliveries WHERE reminder_id=? AND state='accepted'`, claim.ID).Scan(&accepted); err != nil {
		return err
	}
	state, next := "pending", now.Add(32*time.Minute)
	var acceptedAt any
	if pending == 0 && accepted > 0 {
		state, acceptedAt = "delivered", now
	} else {
		if available.Valid {
			next = available.Time
		}
		if next.Before(now.Add(time.Minute)) {
			next = now.Add(time.Minute)
		}
		if !next.Before(claim.Expires) {
			state = "cancelled"
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state=?,available_at=?,delivered_at=?,lease_token=NULL,lease_until=NULL WHERE reminder_id=?`, state, next, acceptedAt, claim.ID); err != nil {
		return err
	}
	return tx.Commit()
}
