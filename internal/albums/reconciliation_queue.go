package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const reconciliationAttempts = 3
const reconciliationLease = 16 * time.Minute

type ReconciliationJob struct {
	OwnerID, UploadID, ResourceID, OperationID string
	Token                                      string
	Attempt                                    int
}

func (r *MySQLUploads) PendingReconciliations(ctx context.Context, now time.Time, limit int) ([]ReconciliationJob, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT u.owner_id,c.upload_id,c.resource_id,c.operation_id
 FROM album_multipart_completions c JOIN album_uploads u ON u.upload_id=c.upload_id
 WHERE c.state IN ('completing','unknown') AND c.reconcile_attempts<? AND c.updated_at<=?
 AND (c.reconcile_after IS NULL OR c.reconcile_after<=?) AND (c.reconcile_until IS NULL OR c.reconcile_until<=?)
 ORDER BY COALESCE(c.reconcile_after,c.updated_at),c.upload_id,c.resource_id LIMIT ?`, reconciliationAttempts, now.Add(-2*time.Minute), now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ReconciliationJob, 0, limit)
	for rows.Next() {
		var job ReconciliationJob
		if err := rows.Scan(&job.OwnerID, &job.UploadID, &job.ResourceID, &job.OperationID); err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

// Attempts are consumed before external reads, including a worker crash. No
// automatic reset after exhaustion; the unresolved media remains recoverable.
func (r *MySQLUploads) ClaimReconciliation(ctx context.Context, job ReconciliationJob, now time.Time) (ReconciliationJob, error) {
	if !validOwner(job.OwnerID) || !validOwner(job.UploadID) || !validOwner(job.OperationID) || !resourceIDPattern.MatchString(job.ResourceID) {
		return ReconciliationJob{}, ErrInvalidManifest
	}
	var result ReconciliationJob
	_, err := r.mutate(ctx, job.OwnerID, job.UploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		var operation, state string
		var attempt int
		var updated time.Time
		var after, until sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT operation_id,state,reconcile_attempts,updated_at,reconcile_after,reconcile_until FROM album_multipart_completions WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, job.ResourceID).Scan(&operation, &state, &attempt, &updated, &after, &until)
		if errors.Is(err, sql.ErrNoRows) {
			return u, ErrNotFound
		}
		if err != nil {
			return u, err
		}
		if operation != job.OperationID || (state != "completing" && state != "unknown") || attempt >= reconciliationAttempts || updated.After(now.Add(-2*time.Minute)) || (after.Valid && after.Time.After(now)) || (until.Valid && until.Time.After(now)) {
			return u, ErrLease
		}
		result = ReconciliationJob{OwnerID: job.OwnerID, UploadID: u.ID, ResourceID: job.ResourceID, OperationID: operation, Token: uuid.NewString(), Attempt: attempt + 1}
		_, err = tx.ExecContext(ctx, `UPDATE album_multipart_completions SET reconcile_attempts=reconcile_attempts+1,reconcile_token=?,reconcile_until=?,reconcile_after=? WHERE upload_id=? AND resource_id=?`, result.Token, now.Add(reconciliationLease), now.Add(reconciliationLease+time.Minute), u.ID, job.ResourceID)
		return u, err
	})
	if err != nil {
		return ReconciliationJob{}, err
	}
	return result, nil
}

func (r *MySQLUploads) FinishReconciliation(ctx context.Context, job ReconciliationJob, now time.Time) error {
	if !validOwner(job.OwnerID) || !validOwner(job.UploadID) || !validOwner(job.OperationID) || !validOwner(job.Token) || !resourceIDPattern.MatchString(job.ResourceID) || job.Attempt < 1 || job.Attempt > reconciliationAttempts {
		return ErrInvalidManifest
	}
	delay := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}[job.Attempt-1]
	result, err := r.db.ExecContext(ctx, `UPDATE album_multipart_completions c JOIN album_uploads u ON u.upload_id=c.upload_id
 SET c.reconcile_token=NULL,c.reconcile_until=NULL,c.reconcile_after=?
 WHERE u.owner_id=? AND c.upload_id=? AND c.resource_id=? AND c.operation_id=? AND c.reconcile_token=? AND c.reconcile_attempts=?`, now.Add(delay), job.OwnerID, job.UploadID, job.ResourceID, job.OperationID, job.Token, job.Attempt)
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
