package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/jobs"
)

func (repository *JobRepository) ClaimDispatches(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]jobs.DispatchItem, error) {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, `
        UPDATE ai_jobs AS job
        JOIN job_dispatch_outbox AS outbox ON outbox.job_id = job.id
        SET job.status = 'failed',
            job.failure_category = CASE
                WHEN job.expires_at <= ? THEN 'job_expired'
                WHEN job.attempt_count >= 3 THEN 'worker_attempts_exhausted'
                ELSE 'worker_delivery_exhausted'
            END,
            job.claim_expires_at = NULL,
            job.updated_at = ?
        WHERE (job.expires_at <= ? OR job.attempt_count >= 3 OR outbox.attempts >= 10)
          AND (job.expires_at <= ? OR job.status = 'queued' OR (job.status = 'running' AND job.claim_expires_at <= ?))`,
		now, now, now, now, now,
	); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx, `
        DELETE outbox FROM job_dispatch_outbox AS outbox
        JOIN ai_jobs AS job ON job.id = outbox.job_id
        WHERE job.status = 'failed'
          AND (job.expires_at <= ? OR job.attempt_count >= 3 OR outbox.attempts >= 10)`, now); err != nil {
		return nil, err
	}
	rows, err := transaction.QueryContext(ctx, `
        SELECT outbox.job_id, outbox.attempts, outbox.available_at
        FROM job_dispatch_outbox AS outbox
        JOIN ai_jobs AS job ON job.id = outbox.job_id
        WHERE outbox.available_at <= ?
          AND (outbox.claimed_until IS NULL OR outbox.claimed_until <= ?)
          AND (job.status = 'queued' OR (job.status = 'running' AND job.claim_expires_at <= ?))
          AND job.expires_at > ?
        ORDER BY outbox.available_at
        LIMIT ?
        FOR UPDATE SKIP LOCKED`, now, now, now, now, limit)
	if err != nil {
		return nil, err
	}
	items := make([]jobs.DispatchItem, 0, limit)
	for rows.Next() {
		var item jobs.DispatchItem
		if err := rows.Scan(&item.JobID, &item.Attempts, &item.AvailableAt); err != nil {
			rows.Close()
			return nil, err
		}
		item.Attempts++
		item.ClaimedUntil = now.Add(30 * time.Second)
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, item := range items {
		if _, err := transaction.ExecContext(ctx, `
            UPDATE job_dispatch_outbox
            SET attempts = ?, claimed_until = ?
            WHERE job_id = ?`, item.Attempts, item.ClaimedUntil, item.JobID); err != nil {
			return nil, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return items, nil
}

func (repository *JobRepository) CompleteDispatch(ctx context.Context, jobID string, now time.Time) error {
	_, err := repository.database.ExecContext(ctx, `
        UPDATE job_dispatch_outbox
        SET available_at = ?, claimed_until = NULL
        WHERE job_id = ?`, now.Add(jobClaimLease), jobID)
	return err
}

func (repository *JobRepository) RetryDispatch(
	ctx context.Context,
	jobID string,
	now time.Time,
	category string,
) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	var attempts int
	err = transaction.QueryRowContext(ctx, `
        SELECT attempts FROM job_dispatch_outbox
        WHERE job_id = ? FOR UPDATE`, jobID).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if attempts >= 10 {
		count, err := affectedRows(transaction.ExecContext(ctx, `
            UPDATE ai_jobs
            SET status = 'failed', failure_category = ?, claim_expires_at = NULL, updated_at = ?
            WHERE id = ?
              AND (status = 'queued' OR (status = 'running' AND claim_expires_at <= ?))`, category, now, jobID, now))
		if err != nil {
			return err
		}
		if count == 1 {
			if _, err := transaction.ExecContext(ctx, `DELETE FROM job_dispatch_outbox WHERE job_id = ?`, jobID); err != nil {
				return err
			}
		} else if _, err := transaction.ExecContext(ctx, `
            UPDATE job_dispatch_outbox
            SET available_at = ?, claimed_until = NULL
            WHERE job_id = ?`, now.Add(jobClaimLease), jobID); err != nil {
			return err
		}
	} else {
		backoff := time.Duration(1<<min(attempts, 6)) * time.Second
		if _, err := transaction.ExecContext(ctx, `
            UPDATE job_dispatch_outbox
            SET available_at = ?, claimed_until = NULL
            WHERE job_id = ?`, now.Add(backoff), jobID); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

var _ jobs.OutboxStore = (*JobRepository)(nil)
