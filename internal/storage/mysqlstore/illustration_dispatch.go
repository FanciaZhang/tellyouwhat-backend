package mysqlstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

var _ illustration.TaskRepository = (*IllustrationRepository)(nil)

func (r *IllustrationRepository) GetTask(ctx context.Context, owner, id string, now time.Time) (illustration.Task, error) {
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return illustration.Task{}, err
	}
	defer tx.Rollback()
	task, _, _, err := r.lock(ctx, tx, owner, id)
	if err != nil {
		return illustration.Task{}, err
	}
	if now.Before(task.CreatedAt) || !now.Before(task.ExpiresAt) {
		return illustration.Task{}, illustration.ErrTaskConflict
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, err
	}
	return task, nil
}

func (r *IllustrationRepository) AbortDispatch(ctx context.Context, claimed illustration.Task) (illustration.Task, error) {
	if claimed.State != illustration.TaskRunning || claimed.AppID != r.appID {
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
	if err := task.AbortBeforeDispatch(); err != nil {
		return illustration.Task{}, err
	}
	metadata, err := json.Marshal(task)
	if err != nil {
		return illustration.Task{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE journal_illustration_tasks SET metadata=?,request_ciphertext=NULL,request_nonce=NULL,result_ciphertext=NULL,result_nonce=NULL WHERE app_id=? AND id=? AND owner_id=?`, metadata, r.appID, task.ID, task.OwnerID)
	if err != nil {
		return illustration.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return illustration.Task{}, err
	}
	return task, nil
}
