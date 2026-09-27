package mysqlstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

var _ illustration.QueueRepository = (*IllustrationRepository)(nil)

// Pending returns only undispatched work. Running/uncertain tasks are never
// reclaimed after restart because the upstream request may already be billed.
func (r *IllustrationRepository) Pending(ctx context.Context, now time.Time, limit int) ([]illustration.Task, error) {
	if limit < 1 || limit > 32 {
		return nil, illustration.ErrInput
	}
	rows, err := r.database.QueryContext(ctx, `SELECT metadata FROM journal_illustration_tasks WHERE app_id=? AND expires_at>? AND JSON_UNQUOTE(JSON_EXTRACT(metadata,'$.State'))='queued' ORDER BY expires_at,id LIMIT ?`, r.appID, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := make([]illustration.Task, 0, limit)
	for rows.Next() {
		var data []byte
		var task illustration.Task
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// Expire releases only known-undispatched reservations. In-flight/uncertain
// charges remain reserved for reconciliation while private payloads are erased.
func (r *IllustrationRepository) Expire(ctx context.Context, now time.Time, limit int) error {
	if limit < 1 || limit > 32 {
		return illustration.ErrInput
	}
	rows, err := r.database.QueryContext(ctx, `SELECT owner_id,id FROM journal_illustration_tasks WHERE app_id=? AND expires_at<=? AND (request_ciphertext IS NOT NULL OR result_ciphertext IS NOT NULL OR JSON_UNQUOTE(JSON_EXTRACT(metadata,'$.State'))='queued') ORDER BY expires_at,id LIMIT ?`, r.appID, now, limit)
	if err != nil {
		return err
	}
	type identity struct{ owner, id string }
	var ids []identity
	for rows.Next() {
		var id identity
		if err := rows.Scan(&id.owner, &id.id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := r.expireOne(ctx, id.owner, id.id, now); err != nil {
			return err
		}
	}
	return nil
}

func (r *IllustrationRepository) expireOne(ctx context.Context, owner, id string, now time.Time) error {
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, _, _, err := r.lock(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	if now.Before(task.ExpiresAt) {
		return nil
	}
	before := task
	if err := task.Cancel(task.Revision); err != nil {
		return err
	}
	if err := r.settleImage(ctx, tx, before, task); err != nil {
		return err
	}
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_illustration_tasks SET metadata=?,request_ciphertext=NULL,request_nonce=NULL,result_ciphertext=NULL,result_nonce=NULL WHERE app_id=? AND owner_id=? AND id=?`, data, r.appID, owner, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
