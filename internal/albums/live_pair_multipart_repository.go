package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Private provider state. Neither provider handles nor operation IDs come from a client.
type livePairMultipart struct {
	PairID, ResourceID, IntentID, State, ProviderID, OperationID, ObjectVersion string
}

const liveMultipartColumns = `pair_upload_id,resource_id,intent_id,state,provider_upload_id,operation_id,object_version`

func scanLiveMultipart(row rowScanner) (livePairMultipart, error) {
	var m livePairMultipart
	var provider, operation, version sql.NullString
	err := row.Scan(&m.PairID, &m.ResourceID, &m.IntentID, &m.State, &provider, &operation, &version)
	m.ProviderID, m.OperationID, m.ObjectVersion = provider.String, operation.String, version.String
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return m, err
}
func lockLiveMultipart(ctx context.Context, tx *sql.Tx, pair, part string) (livePairMultipart, error) {
	return scanLiveMultipart(tx.QueryRowContext(ctx, `SELECT `+liveMultipartColumns+` FROM album_live_pair_multiparts WHERE pair_upload_id=? AND resource_id=? FOR UPDATE`, pair, part))
}
func (r *MySQLUploads) liveMultipart(ctx context.Context, owner, pair, part string) (LivePairUpload, livePairMultipart, error) {
	u, err := r.LivePairByID(ctx, owner, pair)
	if err != nil {
		return u, livePairMultipart{}, err
	}
	if _, err = u.resource(part); err != nil {
		return u, livePairMultipart{}, err
	}
	m, err := scanLiveMultipart(r.db.QueryRowContext(ctx, `SELECT `+liveMultipartColumns+` FROM album_live_pair_multiparts WHERE pair_upload_id=? AND resource_id=?`, pair, part))
	return u, m, err
}
func noDirectLiveUpload(ctx context.Context, tx *sql.Tx, u LivePairUpload) error {
	var issued bool
	if err := tx.QueryRowContext(ctx, `SELECT direct_upload_issued FROM album_live_pairs WHERE pair_upload_id=?`, u.ID).Scan(&issued); err != nil {
		return err
	}
	if issued {
		return ErrConflict
	}
	return nil
}
func (r *MySQLUploads) liveMultipartDeadline(ctx context.Context, tx *sql.Tx, u LivePairUpload, now time.Time) (time.Time, error) {
	if r.billingNow != nil {
		now = maxTime(now, r.billingNow().UTC())
	}
	if u.State != "uploading" {
		return time.Time{}, ErrConflict
	}
	if err := requireVisibleVersion(ctx, tx, u.OwnerID, u.VersionID); err != nil {
		return time.Time{}, err
	}
	if err := noDirectLiveUpload(ctx, tx, u); err != nil {
		return time.Time{}, err
	}
	deadline := minTime(u.ExpiresAt, now.Add(10*time.Minute))
	if r.billingEnvironment != "" {
		account, err := r.cloudAccountInTransaction(ctx, tx, u.OwnerID, now)
		if err != nil {
			return time.Time{}, err
		}
		bound, err := cloudAccountMediaDeadline(account)
		if err != nil {
			return time.Time{}, err
		}
		deadline = minTime(deadline, bound)
	}
	if !now.Before(deadline) {
		return time.Time{}, ErrExpired
	}
	return deadline, nil
}
func (r *MySQLUploads) beginLiveMultipart(ctx context.Context, owner, pair, part string, now time.Time) (LivePairUpload, livePairMultipart, bool, error) {
	var m livePairMultipart
	fresh := false
	u, err := r.mutateLivePair(ctx, owner, pair, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if _, err := u.resource(part); err != nil {
			return u, err
		}
		if _, err := r.liveMultipartDeadline(ctx, tx, u, now); err != nil {
			return u, err
		}
		old, err := lockLiveMultipart(ctx, tx, pair, part)
		if err == nil {
			m = old
			return u, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return u, err
		}
		m = livePairMultipart{PairID: pair, ResourceID: part, IntentID: uuid.NewString(), State: "initiating"}
		_, err = tx.ExecContext(ctx, `INSERT INTO album_live_pair_multiparts(pair_upload_id,resource_id,intent_id,state,updated_at) VALUES(?,?,?,'initiating',?)`, pair, part, m.IntentID, now)
		fresh = err == nil
		return u, err
	})
	if err != nil {
		return u, livePairMultipart{}, false, err
	}
	return u, m, fresh, nil
}

// Observations survive cancellation/deletion acceptance; they never reopen grants.
func (r *MySQLUploads) resolveLiveMultipart(ctx context.Context, owner string, expected livePairMultipart, provider string, now time.Time) (livePairMultipart, error) {
	if provider != "" && !validCOSMultipartID(provider) {
		return livePairMultipart{}, ErrInvalidManifest
	}
	var result livePairMultipart
	_, err := r.mutateLivePair(ctx, owner, expected.PairID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		m, err := lockLiveMultipart(ctx, tx, u.ID, expected.ResourceID)
		if err != nil {
			return u, err
		}
		if m.IntentID != expected.IntentID {
			return u, ErrConflict
		}
		if m.ProviderID != "" {
			if provider != m.ProviderID {
				return u, ErrConflict
			}
			result = m
			return u, nil
		}
		m.State = "unknown"
		var value any
		if provider != "" {
			m.State = "ready"
			m.ProviderID = provider
			value = provider
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_live_pair_multiparts SET state=?,provider_upload_id=?,updated_at=? WHERE pair_upload_id=? AND resource_id=?`, m.State, value, now, m.PairID, m.ResourceID)
		result = m
		return u, err
	})
	return result, err
}
func (r *MySQLUploads) authorizeLiveMultipart(ctx context.Context, owner string, expected livePairMultipart, now func() time.Time, sign func(LivePairUpload, time.Time) error) error {
	_, err := r.mutateLivePair(ctx, owner, expected.PairID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		deadline, err := r.liveMultipartDeadline(ctx, tx, u, now().UTC())
		if err != nil {
			return u, err
		}
		m, err := lockLiveMultipart(ctx, tx, u.ID, expected.ResourceID)
		if err != nil {
			return u, err
		}
		if m.State != "ready" || m.IntentID != expected.IntentID || m.ProviderID != expected.ProviderID {
			return u, ErrConflict
		}
		if err = sign(u, deadline); err != nil {
			return u, err
		}
		if !now().Before(deadline) {
			return u, ErrExpired
		}
		return u, ctx.Err()
	})
	return err
}
func (r *MySQLUploads) claimLiveMultipartOperation(ctx context.Context, owner, pair, part string, abort bool, now time.Time) (LivePairUpload, livePairMultipart, bool, error) {
	var m livePairMultipart
	fresh := false
	u, err := r.mutateLivePair(ctx, owner, pair, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		if _, err := u.resource(part); err != nil {
			return u, err
		}
		if abort {
			var deleting bool
			if err := tx.QueryRowContext(ctx, `SELECT deletion_requested_at IS NOT NULL FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&deleting); err != nil {
				return u, err
			}
			if !deleting {
				return u, ErrConflict
			}
			if err := noDirectLiveUpload(ctx, tx, u); err != nil {
				return u, err
			}
		} else if _, err := r.liveMultipartDeadline(ctx, tx, u, now); err != nil {
			return u, err
		}
		var err error
		m, err = lockLiveMultipart(ctx, tx, pair, part)
		if err != nil {
			return u, err
		}
		if abort && m.State == "aborting" {
			fresh = true
			return u, nil
		} // Abort is idempotent for this bound session.
		if m.State != "ready" {
			return u, nil
		}
		m.State = "completing"
		if abort {
			m.State = "aborting"
		}
		m.OperationID = uuid.NewString()
		_, err = tx.ExecContext(ctx, `UPDATE album_live_pair_multiparts SET state=?,operation_id=?,updated_at=? WHERE pair_upload_id=? AND resource_id=?`, m.State, m.OperationID, now, pair, part)
		fresh = err == nil
		return u, err
	})
	if err != nil {
		return u, livePairMultipart{}, false, err
	}
	return u, m, fresh, nil
}
func (r *MySQLUploads) resolveLiveMultipartOperation(ctx context.Context, owner string, expected livePairMultipart, state, version string, now time.Time) (livePairMultipart, error) {
	if state != "assembled" && state != "completion_unknown" && state != "ready" && state != "aborted" || state == "assembled" && !concreteCOSVersion(version) || state != "assembled" && version != "" {
		return livePairMultipart{}, ErrInvalidManifest
	}
	var result livePairMultipart
	_, err := r.mutateLivePair(ctx, owner, expected.PairID, func(tx *sql.Tx, u LivePairUpload) (LivePairUpload, error) {
		m, err := lockLiveMultipart(ctx, tx, u.ID, expected.ResourceID)
		if err != nil {
			return u, err
		}
		if m.IntentID != expected.IntentID || m.ProviderID != expected.ProviderID || m.OperationID != expected.OperationID || !validOwner(m.OperationID) {
			return u, ErrConflict
		}
		if m.State == "assembled" || m.State == "aborted" {
			if state != m.State || version != m.ObjectVersion {
				return u, ErrConflict
			}
			result = m
			return u, nil
		}
		if state == "aborted" {
			if m.State != "aborting" {
				return u, ErrConflict
			}
		} else if m.State != "completing" && m.State != "completion_unknown" {
			return u, ErrConflict
		}
		// A preflight failure only releases its own still-running attempt.
		if state == "ready" && (m.State != "completing" || expected.State != "completing") {
			return u, ErrConflict
		}
		var op any = m.OperationID
		var v any
		if state == "ready" {
			op = nil
			m.OperationID = ""
		}
		if version != "" {
			v = version
		}
		_, err = tx.ExecContext(ctx, `UPDATE album_live_pair_multiparts SET state=?,operation_id=?,object_version=?,updated_at=? WHERE pair_upload_id=? AND resource_id=?`, state, op, v, now, m.PairID, m.ResourceID)
		m.State, m.ObjectVersion = state, version
		result = m
		return u, err
	})
	return result, err
}
