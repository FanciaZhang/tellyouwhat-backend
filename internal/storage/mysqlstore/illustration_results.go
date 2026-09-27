package mysqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

func illustrationResultAAD(task illustration.Task) []byte {
	b, _ := json.Marshal([]string{"journal-illustration-result-v1", task.AppID, task.OwnerID, task.ID, task.RequestDigest, task.ResultDigest})
	return b
}

func resultMatches(task illustration.Task, result illustration.Result) bool {
	digest := sha256.Sum256(result.Image)
	return len(result.Image) > 0 && len(result.Image) <= illustration.MaxImageBytes && task.ResultDigest == hex.EncodeToString(digest[:])
}

// StoreOutcome is an internal worker operation, never a client-provided task.
// Image allowance settlement and encrypted delivery are committed together.
// Upstream monetary billing reconciliation is separate from this image counter.
func (r *IllustrationRepository) StoreOutcome(ctx context.Context, claimed illustration.Task, result *illustration.Result, failure error, now time.Time) (illustration.Task, error) {
	if claimed.AppID != r.appID || claimed.State != illustration.TaskRunning || claimed.Charge != illustration.ChargeUncertain {
		return illustration.Task{}, illustration.ErrTaskConflict
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Task{}, err
	}
	defer tx.Rollback()
	task, _, _, err := r.lock(ctx, tx, claimed.OwnerID, claimed.ID)
	if err != nil {
		return illustration.Task{}, err
	}
	if !task.SameRequest(claimed) || task.Model != claimed.Model || task.Revision < claimed.Revision {
		return illustration.Task{}, illustration.ErrTaskConflict
	}
	if task.State == illustration.TaskSucceeded && now.Before(task.ExpiresAt) && result != nil && failure == nil && resultMatches(task, *result) {
		// Lost commit acknowledgements may repeat the identical callback.
		return task, nil
	}
	before := task
	deliver, err := task.RecordOutcome(task.Revision, result, failure, now)
	if err != nil {
		return illustration.Task{}, err
	}
	if err := r.settleImage(ctx, tx, before, task); err != nil {
		return illustration.Task{}, err
	}
	var encrypted, nonce []byte
	if deliver {
		encoded, err := json.Marshal(result)
		if err != nil {
			return illustration.Task{}, err
		}
		encrypted, nonce, err = r.cipher.Encrypt(encoded, illustrationResultAAD(task))
		if err != nil {
			return illustration.Task{}, err
		}
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		return illustration.Task{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_illustration_tasks SET metadata=?,request_ciphertext=NULL,request_nonce=NULL,result_ciphertext=?,result_nonce=? WHERE app_id=? AND id=? AND owner_id=?`, metadata, encrypted, nonce, r.appID, task.ID, task.OwnerID)
	if err != nil {
		return illustration.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, err
	}
	return task, nil
}

// Result must be gated by current consent/authorization by the service layer.
// Cancellation and this read are serialized; callers must discard a response
// if client-side access is revoked after it has left the database transaction.
func (r *IllustrationRepository) Result(ctx context.Context, owner, id string, now time.Time) (illustration.Result, error) {
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Result{}, err
	}
	defer tx.Rollback()
	task, _, _, err := r.lock(ctx, tx, owner, id)
	if err != nil {
		return illustration.Result{}, err
	}
	if task.State != illustration.TaskSucceeded || now.Before(task.CreatedAt) || !now.Before(task.ExpiresAt) {
		return illustration.Result{}, illustration.ErrTaskConflict
	}
	var encrypted, nonce []byte
	err = tx.QueryRowContext(ctx, `SELECT result_ciphertext,result_nonce FROM journal_illustration_tasks WHERE app_id=? AND id=? AND owner_id=?`, r.appID, id, owner).Scan(&encrypted, &nonce)
	if err != nil {
		return illustration.Result{}, err
	}
	if len(nonce) != 12 || len(encrypted) > 34_000_000 {
		return illustration.Result{}, illustration.ErrResult
	}
	plaintext, err := r.cipher.Decrypt(encrypted, nonce, illustrationResultAAD(task))
	if err != nil {
		return illustration.Result{}, err
	}
	var result illustration.Result
	if json.Unmarshal(plaintext, &result) != nil || !resultMatches(task, result) {
		return illustration.Result{}, illustration.ErrResult
	}
	if err := tx.Commit(); err != nil {
		return illustration.Result{}, err
	}
	return result, nil
}
