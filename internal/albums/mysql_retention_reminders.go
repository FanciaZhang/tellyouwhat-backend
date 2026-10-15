package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Stable ID is the delivery deduplication key, not a claim of provider exactly-once delivery.
type albumRetentionReminder struct {
	ID, Owner, Environment, Token            string
	Deadline, Due, Expires, Available, Until time.Time
	Days, Attempts                           int
	State                                    string
}

func (albumRetentionReminder) String() string   { return "[redacted album retention reminder]" }
func (albumRetentionReminder) GoString() string { return "[redacted album retention reminder]" }

func reminderWindow(deadline time.Time, days int) (time.Time, time.Time, error) {
	next := 0
	switch days {
	case 30:
		next = 7
	case 7:
		next = 1
	case 1:
	default:
		return time.Time{}, time.Time{}, ErrSubscriptionVerification
	}
	if deadline.IsZero() || deadline.Year() < 2000 || deadline.Year() > 9999 {
		return time.Time{}, time.Time{}, ErrSubscriptionVerification
	}
	return deadline.UTC().AddDate(0, 0, -days), deadline.UTC().AddDate(0, 0, -next), nil
}
func reminderAccountEligible(a AlbumCloudAccount, deadline time.Time) bool {
	return a.State == "expired" && a.RetentionDeadline != nil && a.RetentionDeadline.Equal(deadline) && (a.UsedBytes > 0 || a.ReservedBytes > 0)
}

// Called under the account lock, in the same transaction as verified subscription changes.
func (r *MySQLUploads) syncRetentionReminders(ctx context.Context, tx *sql.Tx, a AlbumCloudAccount, now time.Time) error {
	if r.billingEnvironment == "" || (r.productionSandboxQuota > 0 && r.billingEnvironment == "Sandbox") {
		return nil
	}
	if err := requireMediaAccount(ctx, tx, a.AccountID); err != nil {
		if errors.Is(err, ErrOwner) {
			return nil
		}
		return err
	}
	var deadline time.Time
	if a.RetentionDeadline != nil {
		deadline = *a.RetentionDeadline
	}
	if !reminderAccountEligible(a, deadline) {
		_, err := tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state='cancelled',lease_token=NULL,lease_until=NULL WHERE owner_id=? AND environment=? AND state='pending'`, a.AccountID, r.billingEnvironment)
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state='cancelled',lease_token=NULL,lease_until=NULL WHERE owner_id=? AND environment=? AND state='pending' AND (retention_deadline<>? OR expires_at<=?)`, a.AccountID, r.billingEnvironment, deadline, now); err != nil {
		return err
	}
	for _, days := range []int{30, 7, 1} {
		due, expires, err := reminderWindow(deadline, days)
		if err != nil {
			return err
		}
		if !now.Before(expires) {
			continue
		}
		// Preserve delivered receipts and live leases during repeated Apple reconciliation.
		_, err = tx.ExecContext(ctx, `INSERT INTO album_retention_reminders(reminder_id,owner_id,environment,retention_deadline,days_before,due_at,expires_at,available_at,state) VALUES(?,?,?,?,?,?,?,?,'pending') ON DUPLICATE KEY UPDATE available_at=IF(state='cancelled',VALUES(available_at),available_at),state=IF(state='cancelled','pending',state)`, uuid.NewString(), a.AccountID, r.billingEnvironment, deadline, days, due, expires, due)
		if err != nil {
			return err
		}
	}
	return nil
}

const reminderColumns = `reminder_id,owner_id,environment,retention_deadline,days_before,due_at,expires_at,available_at,state,lease_token,lease_until,attempts`

func scanReminder(row rowScanner) (albumRetentionReminder, error) {
	var c albumRetentionReminder
	var token sql.NullString
	var until sql.NullTime
	err := row.Scan(&c.ID, &c.Owner, &c.Environment, &c.Deadline, &c.Days, &c.Due, &c.Expires, &c.Available, &c.State, &token, &until, &c.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Token = token.String
	c.Until = until.Time
	due, expires, err := reminderWindow(c.Deadline, c.Days)
	if err != nil || !validOwner(c.ID) || !validOwner(c.Owner) || (c.Environment != "Sandbox" && c.Environment != "Production") || !c.Due.Equal(due) || !c.Expires.Equal(expires) || c.Available.Before(due) || c.Attempts < 0 || c.Attempts > 255 || token.Valid != until.Valid || (token.Valid && (!validOwner(c.Token) || c.Attempts < 1 || c.State != "pending" || !c.Until.After(c.Due) || c.Until.After(c.Expires))) || (c.State != "pending" && c.State != "delivered" && c.State != "cancelled") {
		return c, ErrSubscriptionVerification
	}
	return c, nil
}
func (r *MySQLUploads) claimRetentionReminder(ctx context.Context, now time.Time) (albumRetentionReminder, bool, error) {
	if (r.billingEnvironment != "Sandbox" && r.billingEnvironment != "Production") || now.IsZero() {
		return albumRetentionReminder{}, false, ErrSubscriptionUnavailable
	}
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id,reminder_id FROM album_retention_reminders WHERE environment=? AND state='pending' AND available_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY available_at,reminder_id LIMIT 32`, r.billingEnvironment, now, now)
	if err != nil {
		return albumRetentionReminder{}, false, err
	}
	var candidates [][2]string
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			rows.Close()
			return albumRetentionReminder{}, false, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return albumRetentionReminder{}, false, err
	}
	for _, c := range candidates {
		claim, found, err := r.claimReminderCandidate(ctx, c[0], c[1], now)
		if err != nil || found {
			return claim, found, err
		}
	}
	return albumRetentionReminder{}, false, nil
}
func (r *MySQLUploads) claimReminderCandidate(ctx context.Context, owner, id string, now time.Time) (albumRetentionReminder, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return albumRetentionReminder{}, false, err
	}
	defer tx.Rollback()
	// All subscription and reminder mutations lock the account before the reminder.
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT owner_id FROM album_storage_accounts WHERE owner_id=? FOR UPDATE SKIP LOCKED`, owner).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return albumRetentionReminder{}, false, nil
	}
	if err != nil {
		return albumRetentionReminder{}, false, err
	}
	c, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM album_retention_reminders WHERE reminder_id=? AND owner_id=? AND environment=? FOR UPDATE`, id, owner, r.billingEnvironment))
	if err != nil {
		return c, false, err
	}
	if c.State != "pending" || c.Available.After(now) || now.Before(c.Until) {
		return c, false, nil
	}
	a, err := r.cloudAccountInTransaction(ctx, tx, owner, now)
	if err != nil {
		return c, false, err
	}
	if !now.Before(c.Expires) || !reminderAccountEligible(a, c.Deadline) {
		if _, err := tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state='cancelled',lease_token=NULL,lease_until=NULL WHERE reminder_id=?`, id); err != nil {
			return c, false, err
		}
		return c, false, tx.Commit()
	}
	c.Token = uuid.NewString()
	c.Until = minTime(now.UTC().Add(time.Minute), c.Expires)
	c.Attempts = min(c.Attempts+1, 255)
	if _, err := tx.ExecContext(ctx, `UPDATE album_retention_reminders SET lease_token=?,lease_until=?,attempts=? WHERE reminder_id=?`, c.Token, c.Until, c.Attempts, id); err != nil {
		return c, false, err
	}
	return c, true, tx.Commit()
}
func (r *MySQLUploads) finishRetentionReminder(ctx context.Context, claim albumRetentionReminder, delivered bool, now time.Time) error {
	if !validOwner(claim.ID) || !validOwner(claim.Owner) || !validOwner(claim.Token) || claim.Environment != r.billingEnvironment || now.IsZero() {
		return ErrLease
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := lockAccount(ctx, tx, claim.Owner); err != nil {
		return err
	}
	c, err := scanReminder(tx.QueryRowContext(ctx, `SELECT `+reminderColumns+` FROM album_retention_reminders WHERE reminder_id=? AND owner_id=? AND environment=? FOR UPDATE`, claim.ID, claim.Owner, r.billingEnvironment))
	if err != nil {
		return err
	}
	if now.Before(c.Due) || c.State != "pending" || c.Token != claim.Token || !now.Before(c.Until) || !c.Deadline.Equal(claim.Deadline) || c.Days != claim.Days {
		return ErrLease
	}
	a, err := r.cloudAccountInTransaction(ctx, tx, c.Owner, now)
	if err != nil {
		return err
	}
	if !reminderAccountEligible(a, c.Deadline) || !now.Before(c.Expires) {
		return ErrLease
	}
	if delivered {
		_, err = tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state='delivered',delivered_at=?,lease_token=NULL,lease_until=NULL WHERE reminder_id=?`, now, c.ID)
	} else {
		available := now.UTC().Add(time.Minute * time.Duration(1<<min(c.Attempts-1, 5)))
		state := "pending"
		if !available.Before(c.Expires) {
			state = "cancelled"
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_retention_reminders SET state=?,available_at=?,lease_token=NULL,lease_until=NULL WHERE reminder_id=?`, state, available, c.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
