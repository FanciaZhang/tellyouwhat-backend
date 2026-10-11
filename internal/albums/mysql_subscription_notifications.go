package albums

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

func (r *MySQLUploads) validateNotification(n albumNotificationReceipt) error {
	if !validOwner(n.ID) || n.Environment != r.billingEnvironment || (n.Environment != "Sandbox" && n.Environment != "Production") ||
		n.Kind == "" || len(n.Kind) > 64 || n.SignedAt.Year() < 2000 || n.SignedAt.Year() > 9998 || n.Digest == [32]byte{} {
		return ErrSubscriptionVerification
	}
	if n.Kind == "TEST" {
		if n.Owner != "" || n.Original != "" {
			return ErrSubscriptionBinding
		}
	} else if !validOwner(n.Owner) || n.Original == "" || len(n.Original) > 128 {
		return ErrSubscriptionBinding
	}
	return nil
}

func notificationAlreadyProcessed(row rowScanner, n albumNotificationReceipt) (bool, error) {
	var digest []byte
	err := row.Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(digest, n.Digest[:]) {
		return false, ErrSubscriptionVerification
	}
	return true, nil
}

func (r *MySQLUploads) NotificationProcessed(ctx context.Context, n albumNotificationReceipt) (bool, error) {
	if err := r.validateNotification(n); err != nil {
		return false, err
	}
	return notificationAlreadyProcessed(r.db.QueryRowContext(ctx, `SELECT payload_digest FROM album_subscription_notifications WHERE environment=? AND notification_id=?`, n.Environment, n.ID), n)
}

func insertAlbumNotification(ctx context.Context, tx *sql.Tx, n albumNotificationReceipt, now time.Time) error {
	var owner, original any
	if n.Owner != "" {
		owner, original = n.Owner, n.Original
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO album_subscription_notifications(environment,notification_id,notification_type,payload_digest,owner_id,original_transaction_id,signed_at,processed_at) VALUES(?,?,?,?,?,?,?,?)`, n.Environment, n.ID, n.Kind, n.Digest[:], owner, original, n.SignedAt.UTC(), now.UTC())
	return err
}

func (r *MySQLUploads) ApplySubscriptionNotification(ctx context.Context, n albumNotificationReceipt, revision int64, v VerifiedAlbumSubscription, now time.Time) error {
	if err := r.validateNotification(n); err != nil {
		return err
	}
	if n.Kind == "TEST" || v.Environment != n.Environment || v.OriginalTransactionID != n.Original {
		return ErrSubscriptionBinding
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Account always precedes chain/notification locks, including the worker.
	if _, err := lockAccount(ctx, tx, n.Owner); err != nil {
		return err
	}
	done, err := notificationAlreadyProcessed(tx.QueryRowContext(ctx, `SELECT payload_digest FROM album_subscription_notifications WHERE environment=? AND notification_id=? FOR UPDATE`, n.Environment, n.ID), n)
	if err != nil || done {
		return err
	}
	if _, err := r.applySubscriptionInTransaction(ctx, tx, n.Owner, revision, v, now); err != nil {
		return err
	}
	if err := insertAlbumNotification(ctx, tx, n, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *MySQLUploads) RecordSubscriptionTestNotification(ctx context.Context, n albumNotificationReceipt, now time.Time) error {
	if err := r.validateNotification(n); err != nil {
		return err
	}
	if n.Kind != "TEST" {
		return ErrSubscriptionVerification
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	done, err := notificationAlreadyProcessed(tx.QueryRowContext(ctx, `SELECT payload_digest FROM album_subscription_notifications WHERE environment=? AND notification_id=? FOR UPDATE`, n.Environment, n.ID), n)
	if err != nil || done {
		return err
	}
	if err := insertAlbumNotification(ctx, tx, n, now); err != nil {
		return err
	}
	return tx.Commit()
}
