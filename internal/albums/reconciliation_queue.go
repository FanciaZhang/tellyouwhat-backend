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
	Kind                                       string
	Token                                      string
	Attempt                                    int
}

// SQL identifiers come only from these two internal job kinds.
func reconciliationTable(kind string) (table, operation, pending string, err error) {
	switch kind {
	case "initialization":
		return "album_multipart_intents", "intent_id", "initiating", nil
	case "completion":
		return "album_multipart_completions", "operation_id", "completing", nil
	default:
		return "", "", "", ErrInvalidManifest
	}
}

func (r *MySQLUploads) PendingReconciliations(ctx context.Context, now time.Time, limit int) ([]ReconciliationJob, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT owner_id,upload_id,resource_id,operation_id,kind FROM (
 SELECT u.owner_id,c.upload_id,c.resource_id,c.operation_id,'completion' AS kind,COALESCE(c.reconcile_after,c.updated_at) AS available
 FROM album_multipart_completions c JOIN album_uploads u ON u.upload_id=c.upload_id
 WHERE NOT EXISTS (SELECT 1 FROM album_storage_accounts a WHERE a.owner_id=u.owner_id AND a.deletion_requested_at IS NOT NULL) AND c.state IN ('completing','unknown') AND c.reconcile_attempts<? AND c.updated_at<=?
 AND (c.reconcile_after IS NULL OR c.reconcile_after<=?) AND (c.reconcile_until IS NULL OR c.reconcile_until<=?)
 UNION ALL
 SELECT u.owner_id,i.upload_id,i.resource_id,i.intent_id,'initialization',COALESCE(i.reconcile_after,i.updated_at)
 FROM album_multipart_intents i JOIN album_uploads u ON u.upload_id=i.upload_id
 WHERE NOT EXISTS (SELECT 1 FROM album_storage_accounts a WHERE a.owner_id=u.owner_id AND a.deletion_requested_at IS NOT NULL) AND i.state IN ('initiating','unknown') AND i.reconcile_attempts<? AND i.updated_at<=?
 AND (i.reconcile_after IS NULL OR i.reconcile_after<=?) AND (i.reconcile_until IS NULL OR i.reconcile_until<=?)
 ) jobs ORDER BY available,upload_id,resource_id,kind LIMIT ?`, reconciliationAttempts, now.Add(-2*time.Minute), now, now, reconciliationAttempts, now.Add(-2*time.Minute), now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ReconciliationJob, 0, limit)
	for rows.Next() {
		var job ReconciliationJob
		if err := rows.Scan(&job.OwnerID, &job.UploadID, &job.ResourceID, &job.OperationID, &job.Kind); err != nil {
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
	table, column, pending, err := reconciliationTable(job.Kind)
	if err != nil {
		return ReconciliationJob{}, err
	}
	var result ReconciliationJob
	_, err = r.mutate(ctx, job.OwnerID, job.UploadID, func(tx *sql.Tx, u Upload) (Upload, error) {
		if err := requireMediaAccount(ctx, tx, job.OwnerID); err != nil {
			if errors.Is(err, ErrOwner) {
				return u, ErrLease
			}
			return u, err
		}
		var operation, state string
		var attempt int
		var updated time.Time
		var after, until sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT `+column+`,state,reconcile_attempts,updated_at,reconcile_after,reconcile_until FROM `+table+` WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, job.ResourceID).Scan(&operation, &state, &attempt, &updated, &after, &until)
		if errors.Is(err, sql.ErrNoRows) {
			return u, ErrNotFound
		}
		if err != nil {
			return u, err
		}
		if operation != job.OperationID || (state != pending && state != "unknown") || attempt >= reconciliationAttempts || updated.After(now.Add(-2*time.Minute)) || (after.Valid && after.Time.After(now)) || (until.Valid && until.Time.After(now)) {
			return u, ErrLease
		}
		result = ReconciliationJob{OwnerID: job.OwnerID, UploadID: u.ID, ResourceID: job.ResourceID, OperationID: operation, Kind: job.Kind, Token: uuid.NewString(), Attempt: attempt + 1}
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET reconcile_attempts=reconcile_attempts+1,reconcile_token=?,reconcile_until=?,reconcile_after=? WHERE upload_id=? AND resource_id=?`, result.Token, now.Add(reconciliationLease), now.Add(reconciliationLease+time.Minute), u.ID, job.ResourceID)
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
	table, column, _, err := reconciliationTable(job.Kind)
	if err != nil {
		return err
	}
	delay := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}[job.Attempt-1]
	result, err := r.db.ExecContext(ctx, `UPDATE `+table+` c JOIN album_uploads u ON u.upload_id=c.upload_id
 SET c.reconcile_token=NULL,c.reconcile_until=NULL,c.reconcile_after=?
 WHERE u.owner_id=? AND c.upload_id=? AND c.resource_id=? AND c.`+column+`=? AND c.reconcile_token=? AND c.reconcile_attempts=?`, now.Add(delay), job.OwnerID, job.UploadID, job.ResourceID, job.OperationID, job.Token, job.Attempt)
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
