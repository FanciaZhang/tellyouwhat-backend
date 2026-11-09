package albums

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

const deletionMultipartLease = 20 * time.Minute

type deletionMultipartCheck struct {
	account, request, upload, resource, intent, lease, abort, action string
	attempts                                                         int
	parent                                                           Upload
	multipart                                                        MultipartIntent
	completion                                                       MultipartCompletion
}

func (r *MySQLUploads) runMultipartDeletionBatch(ctx context.Context, provider multipartRecoveryProvider, limit int, now func() time.Time) (AccountMediaObservationBatch, error) {
	var result AccountMediaObservationBatch
	if provider == nil || now == nil || limit < 1 || limit > 100 || now().IsZero() {
		return result, ErrInvalidManifest
	}
	service, err := NewMultipartService(r, provider, now)
	if err != nil {
		return result, err
	}
	at := now().UTC()
	rows, err := r.db.QueryContext(ctx, `SELECT u.owner_id,d.request_id,i.upload_id,i.resource_id,i.intent_id FROM album_multipart_intents i JOIN album_uploads u ON u.upload_id=i.upload_id JOIN album_storage_accounts a ON a.owner_id=u.owner_id JOIN album_account_deletions d ON d.account_id=u.owner_id LEFT JOIN album_multipart_completions c ON c.upload_id=i.upload_id AND c.resource_id=i.resource_id WHERE a.deletion_requested_at=d.requested_at AND u.direct_upload_issued=FALSE AND i.deletion_aborted_at IS NULL AND (c.state IS NULL OR c.state<>'assembled') AND (i.deletion_check_after IS NULL OR i.deletion_check_after<=?) AND (i.deletion_check_until IS NULL OR i.deletion_check_until<=?) AND (i.reconcile_until IS NULL OR i.reconcile_until<=?) AND (c.reconcile_until IS NULL OR c.reconcile_until<=?) ORDER BY COALESCE(i.deletion_check_after,d.requested_at),i.upload_id,i.resource_id LIMIT ?`, at, at, at, at, limit)
	if err != nil {
		return result, err
	}
	var jobs []deletionMultipartCheck
	for rows.Next() {
		var c deletionMultipartCheck
		if err = rows.Scan(&c.account, &c.request, &c.upload, &c.resource, &c.intent); err != nil {
			rows.Close()
			return result, err
		}
		jobs = append(jobs, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, candidate := range jobs {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		c, err := r.claimMultipartDeletion(ctx, candidate, now().UTC())
		if errors.Is(err, ErrConflict) {
			result.Contended++
			continue
		}
		if err != nil {
			return result, err
		}
		call, cancel := context.WithTimeout(ctx, 15*time.Minute)
		var callErr error
		switch c.action {
		case "initialization":
			_, callErr = service.reconcileInitialization(call, c.account, c.upload, c.resource, c.intent)
		case "completion":
			_, callErr = service.reconcileCompletion(call, c.account, c.upload, c.resource, c.completion.OperationID)
		case "abort":
			callErr = provider.abortPurgeUpload(call, c.account, purgeTarget{stagingKey(c.parent, c.resource)}, c.multipart.ProviderUploadID)
		default:
			callErr = ErrInvalidManifest
		}
		cancel()
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		acknowledged, finishErr := r.finishMultipartDeletion(persist, c, callErr == nil, now().UTC())
		stop()
		if errors.Is(finishErr, ErrConflict) {
			result.Contended++
		} else if finishErr != nil {
			return result, finishErr
		} else if acknowledged {
			result.Acknowledged++
		} else {
			result.Deferred++
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Lock in deletion order: identity -> storage -> upload -> multipart records.
func (r *MySQLUploads) lockMultipartDeletion(ctx context.Context, tx *sql.Tx, c deletionMultipartCheck) (Upload, MultipartIntent, MultipartCompletion, error) {
	var u Upload
	var m MultipartIntent
	var completion MultipartCompletion
	if !validOwner(c.account) || !validOwner(c.request) || !validOwner(c.upload) || !validOwner(c.intent) || !resourceIDPattern.MatchString(c.resource) {
		return u, m, completion, ErrConflict
	}
	if err := lockDeletingAccount(ctx, tx, c.account, c.request); err != nil {
		return u, m, completion, err
	}
	if _, err := lockAccount(ctx, tx, c.account); err != nil {
		return u, m, completion, err
	}
	var requested, fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT d.requested_at,a.deletion_requested_at FROM album_account_deletions d JOIN album_storage_accounts a ON a.owner_id=d.account_id WHERE d.account_id=? AND d.request_id=?`, c.account, c.request).Scan(&requested, &fenced); err != nil {
		return u, m, completion, err
	}
	if !requested.Valid || !fenced.Valid || !requested.Time.Equal(fenced.Time) {
		return u, m, completion, ErrConflict
	}
	u, err := scanUpload(tx.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM album_uploads WHERE owner_id=? AND upload_id=? FOR UPDATE`, c.account, c.upload))
	if err != nil {
		return u, m, completion, err
	}
	var direct bool
	if err = tx.QueryRowContext(ctx, `SELECT direct_upload_issued FROM album_uploads WHERE upload_id=?`, u.ID).Scan(&direct); err != nil {
		return u, m, completion, err
	}
	if direct || !sealResource(u, c.resource) {
		return u, m, completion, ErrConflict
	}
	m, err = lockMultipart(ctx, tx, u.ID, c.resource)
	if err != nil {
		return u, m, completion, err
	}
	if m.IntentID != c.intent {
		return u, m, completion, ErrConflict
	}
	completion, err = scanMultipartCompletion(tx.QueryRowContext(ctx, `SELECT upload_id,resource_id,operation_id,state,object_version FROM album_multipart_completions WHERE upload_id=? AND resource_id=? FOR UPDATE`, u.ID, c.resource))
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	return u, m, completion, err
}
func (r *MySQLUploads) claimMultipartDeletion(ctx context.Context, c deletionMultipartCheck, at time.Time) (deletionMultipartCheck, error) {
	if at.IsZero() {
		return c, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	c.parent, c.multipart, c.completion, err = r.lockMultipartDeletion(ctx, tx, c)
	if err != nil {
		return c, err
	}
	var oldAbort sql.NullString
	var aborted, normalUntil, completionUntil sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT i.deletion_abort_id,i.deletion_aborted_at,i.reconcile_until,c.reconcile_until FROM album_multipart_intents i LEFT JOIN album_multipart_completions c ON c.upload_id=i.upload_id AND c.resource_id=i.resource_id WHERE i.upload_id=? AND i.resource_id=?`, c.upload, c.resource).Scan(&oldAbort, &aborted, &normalUntil, &completionUntil); err != nil {
		return c, err
	}
	if aborted.Valid || c.completion.State == "assembled" || normalUntil.Valid && normalUntil.Time.After(at) || completionUntil.Valid && completionUntil.Time.After(at) {
		return c, ErrConflict
	}
	c.abort = oldAbort.String
	if c.multipart.State != "ready" {
		if c.completion.State != "" || c.abort != "" {
			return c, ErrConflict
		}
		c.action = "initialization"
	} else {
		if !validCOSMultipartID(c.multipart.ProviderUploadID) {
			return c, ErrConflict
		}
		switch c.completion.State {
		case "", "retryable":
			c.action = "abort"
			if c.abort == "" {
				c.abort = uuid.NewString()
			}
		case "completing", "unknown":
			if c.abort != "" {
				return c, ErrConflict
			}
			c.action = "completion"
		default:
			return c, ErrConflict
		}
	}
	c.lease = uuid.NewString()
	var abort any
	if c.abort != "" {
		abort = c.abort
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_multipart_intents SET deletion_check_token=?,deletion_check_until=?,deletion_check_attempts=LEAST(deletion_check_attempts+1,1000000),deletion_abort_id=? WHERE upload_id=? AND resource_id=? AND (deletion_check_after IS NULL OR deletion_check_after<=?) AND (deletion_check_until IS NULL OR deletion_check_until<=?)`, c.lease, at.Add(deletionMultipartLease), abort, c.upload, c.resource, at, at)
	if err != nil {
		return c, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return c, ErrConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT deletion_check_attempts FROM album_multipart_intents WHERE upload_id=? AND resource_id=?`, c.upload, c.resource).Scan(&c.attempts); err != nil {
		return c, err
	}
	if err = tx.Commit(); err != nil {
		return deletionMultipartCheck{}, err
	}
	return c, nil
}
func (r *MySQLUploads) finishMultipartDeletion(ctx context.Context, c deletionMultipartCheck, observed bool, at time.Time) (bool, error) {
	if !validOwner(c.lease) || c.attempts < 1 || at.IsZero() {
		return false, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	u, m, completion, err := r.lockMultipartDeletion(ctx, tx, c)
	if err != nil {
		return false, err
	}
	if u.Digest != c.parent.Digest {
		return false, ErrConflict
	}
	acknowledged := completion.State == "assembled" && validMultipartVersion(completion.ObjectVersion)
	var receipt any
	if c.action == "abort" {
		var abort sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT deletion_abort_id FROM album_multipart_intents WHERE upload_id=? AND resource_id=?`, c.upload, c.resource).Scan(&abort); err != nil {
			return false, err
		}
		if !validOwner(c.abort) || abort.String != c.abort || m.ProviderUploadID != c.multipart.ProviderUploadID || m.State != "ready" || completion.State != "" && completion.State != "retryable" {
			return false, ErrConflict
		}
		if observed {
			receipt = at
			acknowledged = true
		}
	}
	next := at
	if !observed {
		next = at.Add(accountDeletionRetry(c.attempts))
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_multipart_intents SET deletion_aborted_at=COALESCE(deletion_aborted_at,?),deletion_check_after=?,deletion_check_token=NULL,deletion_check_until=NULL WHERE upload_id=? AND resource_id=? AND intent_id=? AND deletion_check_token=? AND deletion_check_until>?`, receipt, next, c.upload, c.resource, c.intent, c.lease, at)
	if err != nil {
		return false, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return false, ErrConflict
	}
	return acknowledged, tx.Commit()
}
