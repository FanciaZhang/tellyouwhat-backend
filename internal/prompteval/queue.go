package prompteval

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"time"
)

type Claim struct {
	Run   Run
	Index int
	Owner string
}

func (s Store) Claim(ctx context.Context, now time.Time) (Claim, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return Claim{}, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_eval_items WHERE status='running'`).Scan(&active); err != nil {
		return Claim{}, err
	}
	if active >= 2 {
		return Claim{}, ErrNotFound
	}
	var id string
	var index int
	err = tx.QueryRowContext(ctx, `SELECT i.run_id,i.item_index FROM prompt_eval_items i JOIN prompt_eval_runs r ON r.id=i.run_id WHERE i.status='pending' AND r.status IN ('pending','running') AND r.expires_at>? ORDER BY r.created_at,i.item_index LIMIT 1 FOR UPDATE`, now).Scan(&id, &index)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	if err != nil {
		return Claim{}, err
	}
	owner := uuid.NewString()
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_items SET status='running',lease_owner=?,lease_until=? WHERE run_id=? AND item_index=?`, owner, now.Add(45*time.Second), id, index); err != nil {
		return Claim{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_runs SET status='running' WHERE id=?`, id); err != nil {
		return Claim{}, err
	}
	if err = tx.Commit(); err != nil {
		return Claim{}, err
	}
	run, err := s.Get(ctx, id, now)
	return Claim{Run: run, Index: index, Owner: owner}, err
}
func (s Store) Heartbeat(ctx context.Context, c Claim, now time.Time) bool {
	result, err := s.DB.ExecContext(ctx, `UPDATE prompt_eval_items i JOIN prompt_eval_runs r ON r.id=i.run_id SET i.lease_until=? WHERE i.run_id=? AND i.item_index=? AND i.lease_owner=? AND i.status='running' AND r.status='running' AND r.expires_at>?`, now.Add(45*time.Second), c.Run.ID, c.Index, c.Owner, now)
	if err != nil {
		return false
	}
	n, err := result.RowsAffected()
	return err == nil && n == 1
}
func (s Store) SaveResult(ctx context.Context, c Claim, value ItemResult, finished bool) error {
	raw, nonce, err := s.seal(itemKey(c.Run.ID, c.Index), value)
	if err != nil {
		return err
	}
	status := "running"
	if finished {
		status = "completed"
		for _, out := range value.Outputs {
			if out.Error != "" {
				status = "failed"
			}
			for _, check := range out.Checks {
				if !check.Passed {
					status = "failed"
				}
			}
		}
		if value.Error != "" {
			status = "failed"
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE prompt_eval_items SET payload=?,nonce=?,status=? WHERE run_id=? AND item_index=? AND lease_owner=? AND status='running'`, raw, nonce, status, c.Run.ID, c.Index, c.Owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// Recover never repeats an uncertain provider call. Queued items survive worker restarts.
func (s Store) Recover(ctx context.Context, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_items SET status='interrupted' WHERE status='running' AND lease_until<=?`, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_runs SET status='cancelled' WHERE expires_at<=? AND status IN ('pending','running')`, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_items i JOIN prompt_eval_runs r ON r.id=i.run_id SET i.status='cancelled' WHERE i.status='pending' AND r.status='cancelled'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_runs r SET status=IF(EXISTS(SELECT 1 FROM prompt_eval_items i WHERE i.run_id=r.id AND i.status IN ('failed','interrupted')),'completed_with_failures','completed') WHERE r.status IN ('pending','running') AND NOT EXISTS(SELECT 1 FROM prompt_eval_items i WHERE i.run_id=r.id AND i.status IN ('pending','running'))`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.run_id FROM prompt_eval_budget_holds h LEFT JOIN prompt_eval_runs r ON h.run_id=r.id WHERE (r.id IS NULL OR r.status NOT IN ('pending','running')) AND h.remaining_nanos>0`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
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
		if err = releaseHold(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE h FROM prompt_eval_budget_holds h LEFT JOIN prompt_eval_runs r ON r.id=h.run_id WHERE h.remaining_nanos=0 AND (r.id IS NULL OR r.expires_at<=?)`, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM prompt_eval_runs WHERE expires_at<=?`, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM prompt_eval_samples WHERE expires_at<=?`, now); err != nil {
		return err
	}
	return tx.Commit()
}
