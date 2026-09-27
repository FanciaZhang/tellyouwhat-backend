package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/go-sql-driver/mysql"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

// IllustrationRepository is not wired into the shared gateway. Every caller
// must supply its authenticated owner; no operation accepts arbitrary task edits.
type IllustrationRepository struct {
	database  *sql.DB
	cipher    *PayloadCipher
	appID     string
	allowance ImageAllowancePolicy
}

func NewIllustrationRepository(db *sql.DB, cipher *PayloadCipher, appID string, allowance ImageAllowancePolicy) *IllustrationRepository {
	return &IllustrationRepository{database: db, cipher: cipher, appID: appID, allowance: allowance}
}

type illustrationPayload struct {
	Confirmation illustration.Confirmation
	Reference    []byte
}

func illustrationAAD(app, owner, id string) []byte {
	b, _ := json.Marshal([]string{"journal-illustration-request-v1", app, owner, id})
	return b
}

func (r *IllustrationRepository) CreateOrGet(ctx context.Context, owner, model string, confirmation illustration.Confirmation, reference []byte, now time.Time) (illustration.Task, error) {
	task, err := illustration.NewTask(r.appID, owner, model, confirmation, reference, now)
	if err != nil {
		return illustration.Task{}, err
	}
	if r.allowance == nil {
		return illustration.Task{}, ErrImageBudget
	}
	account, limit, err := r.allowance.ImageAllowance(ctx, owner, now)
	if err != nil {
		return illustration.Task{}, err
	}
	task.BillingAccount, task.BudgetMonth = account, now.UTC().Format("2006-01")
	payload, err := json.Marshal(illustrationPayload{confirmation, reference})
	if err != nil {
		return illustration.Task{}, err
	}
	encrypted, nonce, err := r.cipher.Encrypt(payload, illustrationAAD(r.appID, owner, task.ID))
	if err != nil {
		return illustration.Task{}, err
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		return illustration.Task{}, err
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Task{}, err
	}
	defer tx.Rollback()
	// The duplicate branch never changes the stored owner, payload or model.
	_, err = tx.ExecContext(ctx, `INSERT INTO journal_illustration_tasks
		(app_id,id,owner_id,metadata,request_ciphertext,request_nonce,expires_at)
		VALUES (?,?,?,?,?,?,?)`,
		r.appID, task.ID, owner, metadata, encrypted, nonce, task.ExpiresAt)
	inserted := err == nil
	if err != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			return illustration.Task{}, err
		}
	}
	if inserted {
		if err := r.reserveImage(ctx, tx, task, limit); err != nil {
			return illustration.Task{}, err
		}
	}
	stored, _, _, err := r.lock(ctx, tx, owner, task.ID)
	if err != nil {
		return illustration.Task{}, err
	}
	if !stored.SameRequest(task) || stored.BillingAccount != account || !now.Before(stored.ExpiresAt) {
		return illustration.Task{}, illustration.ErrTaskConflict
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, err
	}
	return stored, nil
}

func (r *IllustrationRepository) lock(ctx context.Context, tx *sql.Tx, owner, id string) (illustration.Task, []byte, []byte, error) {
	var raw, encrypted, nonce []byte
	err := tx.QueryRowContext(ctx, `SELECT metadata,request_ciphertext,request_nonce
		FROM journal_illustration_tasks WHERE app_id=? AND id=? AND owner_id=? FOR UPDATE`, r.appID, id, owner).Scan(&raw, &encrypted, &nonce)
	if err != nil {
		return illustration.Task{}, nil, nil, err
	}
	var task illustration.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return illustration.Task{}, nil, nil, err
	}
	if task.AppID != r.appID || task.ID != id || task.OwnerID != owner {
		return illustration.Task{}, nil, nil, illustration.ErrTaskConflict
	}
	return task, encrypted, nonce, nil
}

// Claim commits the one-way dispatch fence before returning any private input.
// If commit acknowledgement is lost, retrying Claim cannot dispatch twice.
func (r *IllustrationRepository) Claim(ctx context.Context, owner, id string, revision uint64, now time.Time) (illustration.Task, illustration.Input, error) {
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	defer tx.Rollback()
	task, encrypted, nonce, err := r.lock(ctx, tx, owner, id)
	if err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	if err := task.Start(revision, now); err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	if len(nonce) != 12 {
		return illustration.Task{}, illustration.Input{}, illustration.ErrTaskConflict
	}
	plaintext, err := r.cipher.Decrypt(encrypted, nonce, illustrationAAD(r.appID, owner, id))
	if err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	var payload illustrationPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	original, err := illustration.NewTask(r.appID, owner, task.Model, payload.Confirmation, payload.Reference, task.CreatedAt)
	if err != nil || !task.SameRequest(original) {
		return illustration.Task{}, illustration.Input{}, illustration.ErrTaskConflict
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE journal_illustration_tasks SET metadata=? WHERE app_id=? AND id=? AND owner_id=?`, metadata, r.appID, id, owner); err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, illustration.Input{}, err
	}
	return task, illustration.Input{Prompt: payload.Confirmation.Prompt, Reference: payload.Reference}, nil
}

// Cancel removes stored private input in the same transaction as cancellation.
// A worker already holding input must still check authorization before dispatch.
func (r *IllustrationRepository) Cancel(ctx context.Context, owner, id string, revision uint64) (illustration.Task, error) {
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Task{}, err
	}
	defer tx.Rollback()
	task, _, _, err := r.lock(ctx, tx, owner, id)
	if err != nil {
		return illustration.Task{}, err
	}
	before := task
	if err := task.Cancel(revision); err != nil {
		return illustration.Task{}, err
	}
	if err := r.settleImage(ctx, tx, before, task); err != nil {
		return illustration.Task{}, err
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		return illustration.Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE journal_illustration_tasks SET metadata=?,request_ciphertext=NULL,request_nonce=NULL,result_ciphertext=NULL,result_nonce=NULL WHERE app_id=? AND id=? AND owner_id=?`, metadata, r.appID, id, owner); err != nil {
		return illustration.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, err
	}
	return task, nil
}
