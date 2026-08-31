package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tellyouwhat/backend/internal/jobs"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/usage"
)

const jobClaimLease = 2 * time.Minute

type JobRepository struct {
	database *sql.DB
	cipher   *PayloadCipher
}

func NewJobRepository(database *sql.DB, cipher *PayloadCipher) *JobRepository {
	return &JobRepository{database: database, cipher: cipher}
}

func (repository *JobRepository) CreateOrGet(ctx context.Context, job jobs.Job) (jobs.Job, error) {
	encoded, err := json.Marshal(job.Request)
	if err != nil {
		return jobs.Job{}, err
	}
	ciphertext, nonce, err := repository.cipher.Encrypt(encoded, []byte("request:"+job.ID))
	if err != nil {
		return jobs.Job{}, err
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return jobs.Job{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
        INSERT INTO ai_jobs
            (id, request_id, body_digest, owner_key_id, owner_device_id, owner_transaction_id,
             request_ciphertext, request_nonce, status, created_at, updated_at, expires_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)`,
		job.ID,
		job.RequestID,
		job.BodyDigest,
		job.OwnerKeyID,
		job.OwnerDeviceID,
		job.OwnerTransactionID,
		ciphertext,
		nonce,
		job.Status,
		job.CreatedAt,
		job.UpdatedAt,
		job.ExpiresAt,
	))
	if err != nil {
		return jobs.Job{}, err
	}
	if count == 1 {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO job_dispatch_outbox (job_id, available_at) VALUES (?, ?)`, job.ID, job.CreatedAt); err != nil {
			return jobs.Job{}, err
		}
		if err := transaction.Commit(); err != nil {
			return jobs.Job{}, err
		}
		return job, nil
	}
	existing, err := repository.scanJob(transaction.QueryRowContext(ctx, jobSelect+` WHERE request_id = ?`, job.RequestID))
	if err != nil {
		return jobs.Job{}, err
	}
	if existing.BodyDigest != job.BodyDigest || existing.OwnerKeyID != job.OwnerKeyID {
		return jobs.Job{}, jobs.ErrIdempotencyConflict
	}
	if existing.Status == jobs.StatusQueued || existing.Status == jobs.StatusRunning {
		if _, err := transaction.ExecContext(ctx, `
            INSERT INTO job_dispatch_outbox (job_id, available_at)
            VALUES (?, ?) ON DUPLICATE KEY UPDATE job_id = VALUES(job_id)`, existing.ID, job.CreatedAt); err != nil {
			return jobs.Job{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return jobs.Job{}, err
	}
	return existing, nil
}

func (repository *JobRepository) Get(ctx context.Context, jobID string) (jobs.Job, error) {
	return repository.scanJob(repository.database.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, jobID))
}

func (repository *JobRepository) Claim(ctx context.Context, jobID string, now time.Time) (jobs.Job, error) {
	count, err := affectedRows(repository.database.ExecContext(ctx, `
        UPDATE ai_jobs
        SET status = 'running', attempt_count = attempt_count + 1,
            claim_expires_at = ?, updated_at = ?
        WHERE id = ?
          AND (status = 'queued' OR (status = 'running' AND claim_expires_at <= ?))
          AND attempt_count < 3 AND expires_at > ?`,
		now.Add(jobClaimLease), now, jobID, now, now,
	))
	if err != nil {
		return jobs.Job{}, err
	}
	if count != 1 {
		return jobs.Job{}, jobs.ErrJobNotClaimable
	}
	return repository.Get(ctx, jobID)
}

func (repository *JobRepository) ExtendLease(ctx context.Context, jobID string, attempt int, now time.Time) error {
	count, err := affectedRows(repository.database.ExecContext(ctx, `
        UPDATE ai_jobs
        SET claim_expires_at = ?, updated_at = ?
        WHERE id = ? AND status = 'running' AND attempt_count = ?`,
		now.Add(jobClaimLease), now, jobID, attempt,
	))
	if err != nil {
		return err
	}
	if count != 1 {
		return jobs.ErrJobNotClaimable
	}
	return nil
}

func (repository *JobRepository) Succeed(
	ctx context.Context,
	jobID string,
	attempt int,
	response providerapi.Response,
	usageRecord usage.Record,
	now time.Time,
) error {
	ciphertext, nonce, err := repository.cipher.Encrypt([]byte(response.Content), []byte("result:"+jobID))
	if err != nil {
		return err
	}
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
        UPDATE ai_jobs
        SET status = 'succeeded', result_ciphertext = ?, result_nonce = ?,
            input_tokens = ?, output_tokens = ?, claim_expires_at = NULL, updated_at = ?
        WHERE id = ? AND status = 'running' AND attempt_count = ?`,
		ciphertext, nonce, response.InputTokens, response.OutputTokens, now, jobID, attempt,
	))
	if err != nil {
		return err
	}
	if count != 1 {
		return jobs.ErrJobNotClaimable
	}
	if usageRecord.RequestID == "" || usageRecord.KeyID == "" {
		return jobs.ErrIdempotencyConflict
	}
	if _, err := transaction.ExecContext(ctx, `
        INSERT INTO usage_ledger
            (request_id, key_id, device_id, original_transaction_id, operation,
             input_tokens, output_tokens, occurred_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE request_id = VALUES(request_id)`,
		usageRecord.RequestID, usageRecord.KeyID, usageRecord.DeviceID, usageRecord.TransactionID,
		usageRecord.Operation, usageRecord.InputTokens, usageRecord.OutputTokens, usageRecord.OccurredAt,
	); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM job_dispatch_outbox WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *JobRepository) Fail(ctx context.Context, jobID string, attempt int, category string, now time.Time) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
        UPDATE ai_jobs
        SET status = 'failed', failure_category = ?, claim_expires_at = NULL, updated_at = ?
        WHERE id = ? AND status = 'running' AND attempt_count = ?`, category, now, jobID, attempt))
	if err != nil {
		return err
	}
	if count != 1 {
		return jobs.ErrJobNotClaimable
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM job_dispatch_outbox WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (repository *JobRepository) RetryOrFail(ctx context.Context, jobID string, attempt int, category string, now time.Time) (bool, error) {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = transaction.Rollback() }()
	var storedAttempt int
	var currentStatus jobs.Status
	err = transaction.QueryRowContext(ctx, `
        SELECT attempt_count, status FROM ai_jobs WHERE id = ? FOR UPDATE`, jobID).Scan(&storedAttempt, &currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, jobs.ErrJobNotClaimable
	}
	if err != nil {
		return false, err
	}
	if currentStatus != jobs.StatusRunning || storedAttempt != attempt {
		return false, jobs.ErrJobNotClaimable
	}
	nextStatus := jobs.StatusQueued
	if storedAttempt >= 3 {
		nextStatus = jobs.StatusFailed
	}
	count, err := affectedRows(transaction.ExecContext(ctx, `
        UPDATE ai_jobs
        SET status = ?, failure_category = ?, claim_expires_at = NULL, updated_at = ?
        WHERE id = ? AND status = 'running' AND attempt_count = ?`,
		nextStatus, category, now, jobID, attempt,
	))
	if err != nil {
		return false, err
	}
	if count != 1 {
		return false, jobs.ErrJobNotClaimable
	}
	if nextStatus == jobs.StatusFailed {
		_, err = transaction.ExecContext(ctx, `DELETE FROM job_dispatch_outbox WHERE job_id = ?`, jobID)
	} else {
		_, err = transaction.ExecContext(ctx, `
            INSERT INTO job_dispatch_outbox (job_id, available_at)
            VALUES (?, ?)
            ON DUPLICATE KEY UPDATE available_at = VALUES(available_at), claimed_until = NULL`, jobID, now)
	}
	if err != nil {
		return false, err
	}
	if err := transaction.Commit(); err != nil {
		return false, err
	}
	return nextStatus == jobs.StatusQueued, nil
}

func (repository *JobRepository) Cancel(ctx context.Context, jobID string, now time.Time) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := affectedRows(transaction.ExecContext(ctx, `
        UPDATE ai_jobs
        SET status = 'cancelled', claim_expires_at = NULL, updated_at = ?
        WHERE id = ? AND status IN ('queued', 'running')`, now, jobID))
	if err != nil {
		return err
	}
	if count != 1 {
		return jobs.ErrJobNotClaimable
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM job_dispatch_outbox WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return transaction.Commit()
}

const jobSelect = `
    SELECT id, request_id, body_digest, owner_key_id, owner_device_id, owner_transaction_id,
           request_ciphertext, request_nonce, status, result_ciphertext, result_nonce,
           input_tokens, output_tokens, failure_category, attempt_count, claim_expires_at,
           created_at, updated_at, expires_at
    FROM ai_jobs`

type rowScanner interface{ Scan(...any) error }

func (repository *JobRepository) scanJob(row rowScanner) (jobs.Job, error) {
	var job jobs.Job
	var requestCiphertext, requestNonce, resultCiphertext, resultNonce []byte
	var claimExpiresAt sql.NullTime
	err := row.Scan(
		&job.ID,
		&job.RequestID,
		&job.BodyDigest,
		&job.OwnerKeyID,
		&job.OwnerDeviceID,
		&job.OwnerTransactionID,
		&requestCiphertext,
		&requestNonce,
		&job.Status,
		&resultCiphertext,
		&resultNonce,
		&job.InputTokens,
		&job.OutputTokens,
		&job.FailureCategory,
		&job.AttemptCount,
		&claimExpiresAt,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, err
	}
	if claimExpiresAt.Valid {
		job.ClaimExpiresAt = claimExpiresAt.Time
	}
	requestJSON, err := repository.cipher.Decrypt(requestCiphertext, requestNonce, []byte("request:"+job.ID))
	if err != nil {
		return jobs.Job{}, fmt.Errorf("decrypt job request: %w", err)
	}
	if err := json.Unmarshal(requestJSON, &job.Request); err != nil {
		return jobs.Job{}, fmt.Errorf("decode job request: %w", err)
	}
	if len(resultCiphertext) > 0 {
		result, err := repository.cipher.Decrypt(resultCiphertext, resultNonce, []byte("result:"+job.ID))
		if err != nil {
			return jobs.Job{}, fmt.Errorf("decrypt job result: %w", err)
		}
		job.Result = string(result)
	}
	return job, nil
}

var _ jobs.Store = (*JobRepository)(nil)
