package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const appleDeletionLease = 2 * time.Minute

type appleDeletionJob struct {
	account, request, grant, lease string
	attempts                       int
}

type appleDeletionBatch struct{ Acknowledged, Deferred, Contended int }

// Internal deletion phase only. Acknowledgement never marks account/media erasure
// complete; the full deletion orchestrator must also close writes and erase data.
func (r *MySQLAccounts) runAppleDeletionBatch(ctx context.Context, revoke func(context.Context, string) error, limit int, now func() time.Time) (appleDeletionBatch, error) {
	var result appleDeletionBatch
	if revoke == nil || now == nil || limit < 1 || limit > 100 {
		return result, ErrInvalidManifest
	}
	at := now().UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return result, ErrInvalidManifest
	}
	rows, err := r.db.QueryContext(ctx, `SELECT account_id,request_id,grant_id FROM album_deletion_apple_jobs WHERE acknowledged_at IS NULL AND available_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY available_at,grant_id LIMIT ?`, at, at, limit)
	if err != nil {
		return result, err
	}
	var jobs []appleDeletionJob
	for rows.Next() {
		var job appleDeletionJob
		if err = rows.Scan(&job.account, &job.request, &job.grant); err != nil {
			rows.Close()
			return result, err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, job := range jobs {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		job, err = r.claimAppleDeletion(ctx, job, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		token, revokeErr := r.RefreshCredential(ctx, job.account, job.grant)
		if revokeErr == nil {
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			revokeErr = revoke(call, token)
			cancel()
		}
		// On cancellation or a lost DB acknowledgement, leave the lease to expire.
		// Retrying an already invalidated Apple token is safe; losing it early is not.
		if err = ctx.Err(); err != nil {
			return result, err
		}
		err = r.finishAppleDeletion(ctx, job, revokeErr == nil, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		if revokeErr == nil {
			result.Acknowledged++
		} else {
			result.Deferred++
		}
	}
	return result, nil
}

func lockAppleDeletionAccount(ctx context.Context, tx *sql.Tx, job appleDeletionJob) error {
	var state, request string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=? FOR UPDATE`, job.account).Scan(&state); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT request_id FROM album_account_deletions WHERE account_id=?`, job.account).Scan(&request); err != nil {
		return err
	}
	if state != "deleting" || request != job.request {
		return ErrConflict
	}
	return nil
}

func (r *MySQLAccounts) claimAppleDeletion(ctx context.Context, job appleDeletionJob, at time.Time) (appleDeletionJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return job, err
	}
	defer tx.Rollback()
	if err = lockAppleDeletionAccount(ctx, tx, job); err != nil {
		return job, err
	}
	job.lease = uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_apple_jobs j JOIN album_apple_grants g ON g.grant_id=j.grant_id AND g.account_id=j.account_id SET j.lease_token=?,j.lease_until=?,j.attempts=LEAST(j.attempts+1,1000000) WHERE j.account_id=? AND j.request_id=? AND j.grant_id=? AND g.revoked_at IS NOT NULL AND j.acknowledged_at IS NULL AND j.available_at<=? AND (j.lease_until IS NULL OR j.lease_until<=?)`, job.lease, at.Add(appleDeletionLease), job.account, job.request, job.grant, at, at)
	if err != nil {
		return job, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return job, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT attempts FROM album_deletion_apple_jobs WHERE grant_id=?`, job.grant).Scan(&job.attempts); err != nil {
		return job, err
	}
	return job, tx.Commit()
}

func appleDeletionRetry(attempts int) time.Duration {
	return min(time.Minute*time.Duration(1<<min(max(attempts-1, 0), 9)), 6*time.Hour)
}

func (r *MySQLAccounts) finishAppleDeletion(ctx context.Context, job appleDeletionJob, acknowledged bool, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockAppleDeletionAccount(ctx, tx, job); err != nil {
		return err
	}
	var receipt any
	if acknowledged {
		receipt = at
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_apple_jobs SET acknowledged_at=?,available_at=?,lease_token=NULL,lease_until=NULL WHERE account_id=? AND request_id=? AND grant_id=? AND lease_token=? AND lease_until>? AND acknowledged_at IS NULL`, receipt, at.Add(appleDeletionRetry(job.attempts)), job.account, job.request, job.grant, job.lease, at)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrConflict
	}
	if acknowledged {
		// Preserve the grant FK for revoked sessions, but erase recoverable credentials
		// in the same commit as the durable provider acknowledgement.
		result, err = tx.ExecContext(ctx, `UPDATE album_apple_grants SET encrypted_token=X'',token_nonce=? WHERE account_id=? AND grant_id=? AND revoked_at IS NOT NULL`, make([]byte, 12), job.account, job.grant)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrConflict
		}
	}
	return tx.Commit()
}
