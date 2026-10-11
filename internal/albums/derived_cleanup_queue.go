package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type DerivedObjectVersion struct {
	VersionID    string `json:"versionID"`
	DeleteMarker bool   `json:"deleteMarker"`
	SizeBytes    int64  `json:"sizeBytes"`
}
type DerivedCleanupPlan struct {
	Terminal bool                   `json:"terminal"`
	Versions []DerivedObjectVersion `json:"versions"`
}
type DerivedCleanup struct {
	ID, OwnerID, VersionID, ManifestDigest, Kind, ReferenceID, KeepVersion, State, LeaseToken string
	ReserveBytes                                                                              int64
	Attempts                                                                                  int
	LeaseUntil                                                                                time.Time
	Plan                                                                                      *DerivedCleanupPlan
}

const cleanupColumns = `cleanup_id,owner_id,version_id,manifest_digest,kind,reference_id,reserve_bytes,keep_version,state,attempts,lease_token,lease_until,plan_json`

func validateCleanupPlan(plan DerivedCleanupPlan) error {
	if !plan.Terminal || len(plan.Versions) > 1000 {
		return ErrConflict
	}
	seen := make(map[string]bool)
	for _, v := range plan.Versions {
		if !concreteCOSVersion(v.VersionID) || v.SizeBytes < 0 || (v.DeleteMarker && v.SizeBytes != 0) || seen[v.VersionID] {
			return ErrConflict
		}
		seen[v.VersionID] = true
	}
	return nil
}
func scanCleanup(row rowScanner) (DerivedCleanup, error) {
	var c DerivedCleanup
	var keep, token sql.NullString
	var lease sql.NullTime
	var plan []byte
	err := row.Scan(&c.ID, &c.OwnerID, &c.VersionID, &c.ManifestDigest, &c.Kind, &c.ReferenceID, &c.ReserveBytes, &keep, &c.State, &c.Attempts, &token, &lease, &plan)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.KeepVersion, c.LeaseToken, c.LeaseUntil = keep.String, token.String, lease.Time
	if !validOwner(c.ID) || !validOwner(c.OwnerID) || !validOwner(c.VersionID) || !validOwner(c.ReferenceID) || !validSHA256(c.ManifestDigest) ||
		(c.Kind != "photo" && c.Kind != "video") || c.Attempts < 0 || c.Attempts > 8 || c.ReserveBytes < 0 ||
		(keep.Valid && (!concreteCOSVersion(c.KeepVersion) || c.ReserveBytes != 0)) || (!keep.Valid && c.ReserveBytes <= 0) ||
		(c.State != "queued" && c.State != "running" && c.State != "completed" && c.State != "attention") {
		return DerivedCleanup{}, ErrConflict
	}
	if len(plan) > 0 {
		c.Plan = &DerivedCleanupPlan{}
		if len(plan) > 262144 || json.Unmarshal(plan, c.Plan) != nil || validateCleanupPlan(*c.Plan) != nil {
			return DerivedCleanup{}, ErrConflict
		}
	}
	return c, nil
}
func cleanupLease(current, expected DerivedCleanup, now time.Time) bool {
	return current.ID == expected.ID && current.OwnerID == expected.OwnerID && current.VersionID == expected.VersionID && current.ManifestDigest == expected.ManifestDigest &&
		current.Kind == expected.Kind && current.ReferenceID == expected.ReferenceID && current.ReserveBytes == expected.ReserveBytes && current.KeepVersion == expected.KeepVersion &&
		current.State == "running" && validOwner(expected.LeaseToken) && current.LeaseToken == expected.LeaseToken && current.LeaseUntil.After(now)
}
func (r *MySQLUploads) DerivedCleanup(ctx context.Context, owner, id string) (DerivedCleanup, error) {
	if !validOwner(owner) || !validOwner(id) {
		return DerivedCleanup{}, ErrInvalidManifest
	}
	return scanCleanup(r.db.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE owner_id=? AND cleanup_id=?`, owner, id))
}
func (r *MySQLUploads) PendingDerivedCleanups(ctx context.Context, now time.Time, limit int) ([]DerivedCleanup, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE (state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?) ORDER BY available_at,cleanup_id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DerivedCleanup
	for rows.Next() {
		c, err := scanCleanup(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}
func (r *MySQLUploads) ClaimDerivedCleanup(ctx context.Context, owner, id, token string, now time.Time) (DerivedCleanup, error) {
	if !validOwner(owner) || !validOwner(id) || !validOwner(token) {
		return DerivedCleanup{}, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedCleanup{}, err
	}
	defer tx.Rollback()
	c, err := scanCleanup(tx.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE owner_id=? AND cleanup_id=? AND ((state='queued' AND available_at<=?) OR (state='running' AND lease_until<=?)) FOR UPDATE`, owner, id, now, now))
	if errors.Is(err, ErrNotFound) {
		return DerivedCleanup{}, ErrLease
	}
	if err != nil {
		return DerivedCleanup{}, err
	}
	if c.Attempts >= 8 {
		_, err = tx.ExecContext(ctx, `UPDATE album_derived_cleanups SET state='attention',lease_token=NULL,lease_until=NULL,updated_at=? WHERE cleanup_id=?`, now, id)
		if err != nil {
			return DerivedCleanup{}, err
		}
		if err = tx.Commit(); err != nil {
			return DerivedCleanup{}, err
		}
		return DerivedCleanup{}, ErrLease
	}
	c.Attempts++
	c.State = "running"
	c.LeaseToken = token
	c.LeaseUntil = now.Add(10 * time.Minute)
	_, err = tx.ExecContext(ctx, `UPDATE album_derived_cleanups SET state='running',attempts=?,lease_token=?,lease_until=?,updated_at=? WHERE cleanup_id=?`, c.Attempts, token, c.LeaseUntil, now, id)
	if err != nil {
		return DerivedCleanup{}, err
	}
	return c, tx.Commit()
}
func (r *MySQLUploads) FinishDerivedCleanupStep(ctx context.Context, expected DerivedCleanup, terminal bool, now time.Time) error {
	state := "queued"
	if terminal || expected.Attempts >= 8 {
		state = "attention"
	}
	if !validOwner(expected.OwnerID) || !validOwner(expected.ID) || !validOwner(expected.LeaseToken) || expected.Attempts < 1 || expected.Attempts > 8 {
		return ErrInvalidManifest
	}
	result, err := r.db.ExecContext(ctx, `UPDATE album_derived_cleanups SET state=?,available_at=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE owner_id=? AND cleanup_id=? AND state='running' AND lease_token=? AND lease_until>?`, state, now.Add(15*time.Minute), now, expected.OwnerID, expected.ID, expected.LeaseToken, now)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLease
	}
	return nil
}

// Called after actual bytes have moved reserved -> used, under the account lock.
// Recovery after closing settles only this intent's retained escrow and queues
// removal of any unreferenced output versions without withdrawing its receipt.
func settleClosedDerivedWrite(ctx context.Context, tx *sql.Tx, owner, version, digest, kind, reference string, held, actual int64, keep string, now time.Time) error {
	f, err := scanFinalization(tx.QueryRowContext(ctx, `SELECT `+finalizationColumns+` FROM album_derived_finalizations WHERE owner_id=? AND version_id=? FOR UPDATE`, owner, version))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if f.ManifestDigest != digest || held <= 0 || actual <= 0 || actual > held || f.HeldBytes < held || !concreteCOSVersion(keep) {
		return ErrConflict
	}
	c, err := scanCleanup(tx.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM album_derived_cleanups WHERE owner_id=? AND kind=? AND reference_id=? FOR UPDATE`, owner, kind, reference))
	if err != nil {
		return err
	}
	if c.VersionID != version || c.ManifestDigest != digest || c.KeepVersion != "" || c.ReserveBytes != held || c.State == "completed" {
		return ErrConflict
	}
	refund := held - actual
	if refund > 0 {
		result, err := tx.ExecContext(ctx, `UPDATE album_storage_accounts SET reserved_bytes=reserved_bytes-? WHERE owner_id=? AND reserved_bytes>=?`, refund, owner, refund)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return ErrQuota
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_derived_finalizations SET retained_bytes=retained_bytes+?,held_bytes=held_bytes-?,released_bytes=released_bytes+?,updated_at=? WHERE owner_id=? AND version_id=?`, actual, held, refund, now, owner, version)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE album_derived_cleanups SET reserve_bytes=0,keep_version=?,state='queued',attempts=0,plan_json=NULL,available_at=?,lease_token=NULL,lease_until=NULL,updated_at=? WHERE cleanup_id=?`, keep, now, now, c.ID)
	return err
}
func (r *MySQLUploads) CheckDerivedSchema(ctx context.Context) error {
	for _, query := range []string{`SELECT derived_budget_closed FROM album_uploads LIMIT 0`, `SELECT ` + finalizationColumns + ` FROM album_derived_finalizations LIMIT 0`, `SELECT ` + cleanupColumns + ` FROM album_derived_cleanups LIMIT 0`} {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
