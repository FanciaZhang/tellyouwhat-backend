package prompteval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"time"
)

type Store struct {
	DB     *sql.DB
	Cipher *mysqlstore.PayloadCipher
	Limits costcontrol.Limits
}

func lock(ctx context.Context, tx *sql.Tx) error {
	var id int
	return tx.QueryRowContext(ctx, `SELECT singleton_id FROM ai_cost_control_state WHERE singleton_id=1 FOR UPDATE`).Scan(&id)
}
func (s Store) seal(id string, value any) ([]byte, []byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	return s.Cipher.Encrypt(raw, []byte("evaluation:"+id))
}
func (s Store) open(id string, raw, nonce []byte, value any) error {
	data, err := s.Cipher.Decrypt(raw, nonce, []byte("evaluation:"+id))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func (s Store) SaveSample(ctx context.Context, value Sample, actor string, now time.Time) error {
	if value.Validate() != nil || uuid.Validate(actor) != nil {
		return ErrInvalid
	}
	raw, nonce, err := s.seal(value.ID, value)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	var previous, previousNonce []byte
	err = tx.QueryRowContext(ctx, `SELECT payload,nonce FROM prompt_eval_samples WHERE id=?`, value.ID).Scan(&previous, &previousNonce)
	if err == nil {
		var existing Sample
		if err = s.open(value.ID, previous, previousNonce, &existing); err != nil {
			return err
		}
		a, _ := json.Marshal(existing)
		b, _ := json.Marshal(value)
		if !bytes.Equal(a, b) {
			return ErrConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_eval_samples(id,payload,nonce,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?)`, value.ID, raw, nonce, actor, now, now.Add(Retention))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,app_id,request_id,action,target_type,target_id,outcome,metadata_json,created_at) VALUES(?,'journal',?,'prompts.sample.save','evaluation_sample',?,'succeeded','{}',?)`, actor, uuid.NewString(), value.ID, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) Samples(ctx context.Context, now time.Time) ([]Sample, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,payload,nonce FROM prompt_eval_samples WHERE expires_at>? ORDER BY created_at DESC LIMIT 200`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var id string
		var raw, nonce []byte
		var value Sample
		if err = rows.Scan(&id, &raw, &nonce); err != nil {
			return nil, err
		}
		if err = s.open(id, raw, nonce, &value); err != nil {
			return nil, err
		}
		out = append(out, Sample{ID: value.ID, Name: value.Name, Kind: value.Kind})
	}
	return out, rows.Err()
}
func (s Store) DeleteSample(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM prompt_eval_samples WHERE id=?`, id)
	return err
}
func (s Store) Start(ctx context.Context, plan Plan, m aiconfig.Mutation, now time.Time) (Run, error) {
	return s.StartInput(ctx, plan, m, plan, now)
}
func (s Store) StartInput(ctx context.Context, plan Plan, m aiconfig.Mutation, submitted any, now time.Time) (Run, error) {
	if !aiconfig.ValidMutation(m) || plan.ReservedNanos <= 0 || len(plan.Samples) == 0 || len(plan.Samples) > 20 || len(plan.Candidates) == 0 || len(plan.Candidates) > 2 {
		return Run{}, ErrInvalid
	}
	rawPlan, _ := json.Marshal(submitted)
	hash := sha256.Sum256(rawPlan)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return Run{}, err
	}
	var oldID string
	var oldHash []byte
	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM prompt_eval_runs WHERE actor_id=? AND idempotency_key=?`, m.Actor, m.Key).Scan(&oldID, &oldHash)
	if err == nil {
		if !bytes.Equal(hash[:], oldHash) {
			return Run{}, ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return Run{}, err
		}
		return s.Get(ctx, oldID, now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	run := Run{ID: uuid.NewString(), Status: "pending", CreatedAt: now, ExpiresAt: now.Add(Retention), Plan: plan, Items: []Item{}}
	revisionIDs := []string{}
	for _, c := range plan.Candidates {
		revisionIDs = append(revisionIDs, c.Revision.ID)
	}
	revisionJSON, _ := json.Marshal(revisionIDs)
	raw, nonce, err := s.seal(run.ID, plan)
	if err != nil {
		return Run{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO prompt_eval_runs(id,actor_id,idempotency_key,request_hash,candidate_revisions,payload,nonce,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,'pending',?,?)`, run.ID, m.Actor, m.Key, hash[:], revisionJSON, raw, nonce, now, run.ExpiresAt)
	if err != nil {
		return Run{}, err
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	attempt := costcontrol.Attempt{ID: run.ID, AppID: "journal", Operation: "journal.evaluation.reserve", Meter: "ark", MonthStart: month, ReservedNanos: plan.ReservedNanos, CreatedAt: now, LeaseExpiresAt: now.Add(time.Hour)}
	if err = mysqlstore.ReserveCostAttempt(ctx, tx, attempt, s.Limits); err != nil {
		return Run{}, err
	}
	// The hold consumes budget, while individual calls acquire the ordinary concurrency slots.
	if _, err = tx.ExecContext(ctx, `DELETE FROM ai_cost_attempts WHERE id=?`, run.ID); err != nil {
		return Run{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO prompt_eval_budget_holds VALUES(?,'journal',?,?,?,?)`, run.ID, month, plan.ReservedNanos, plan.ReservedNanos, run.ExpiresAt); err != nil {
		return Run{}, err
	}
	for index := range plan.Samples {
		if _, err = tx.ExecContext(ctx, `INSERT INTO prompt_eval_items(run_id,item_index,status) VALUES(?,?,'pending')`, run.ID, index); err != nil {
			return Run{}, err
		}
		run.Items = append(run.Items, Item{Index: index, Status: "pending"})
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO admin_audit_events(admin_user_id,request_id,action,target_type,target_id,outcome,metadata_json,created_at) VALUES(?,?,'prompts.evaluate','evaluation',?,'succeeded','{}',?)`, m.Actor, m.RequestID, run.ID, now); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}
func (s Store) Get(ctx context.Context, id string, now time.Time) (Run, error) {
	var r Run
	var raw, nonce []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id,status,created_at,expires_at,payload,nonce FROM prompt_eval_runs WHERE id=? AND expires_at>?`, id, now).Scan(&r.ID, &r.Status, &r.CreatedAt, &r.ExpiresAt, &raw, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err = s.open(id, raw, nonce, &r.Plan); err != nil {
		return r, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT item_index,status FROM prompt_eval_items WHERE run_id=? ORDER BY item_index`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	r.Items = []Item{}
	for rows.Next() {
		var item Item
		if err = rows.Scan(&item.Index, &item.Status); err != nil {
			return r, err
		}
		r.Items = append(r.Items, item)
	}
	return r, rows.Err()
}
func (s Store) List(ctx context.Context, now time.Time) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,status,created_at,expires_at FROM prompt_eval_runs WHERE expires_at>? ORDER BY created_at DESC LIMIT 50`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		if err = rows.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func itemKey(run string, index int) string { return fmt.Sprintf("%s:%d", run, index) }
func (s Store) Cancel(ctx context.Context, id string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_runs SET status='cancelled' WHERE id=? AND status IN ('pending','running')`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_items SET status='cancelled' WHERE run_id=? AND status='pending'`, id); err != nil {
		return err
	}
	if err = releaseHold(ctx, tx, id, now); err != nil {
		return err
	}
	return tx.Commit()
}
func releaseHold(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	var remaining int64
	var month time.Time
	err := tx.QueryRowContext(ctx, `SELECT remaining_nanos,month_start FROM prompt_eval_budget_holds WHERE run_id=?`, id).Scan(&remaining, &month)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if remaining == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_cost_months SET charged_nanos=charged_nanos-?,updated_at=? WHERE month_start=? AND charged_nanos>=?`, remaining, now, month, remaining)
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
	_, err = tx.ExecContext(ctx, `UPDATE prompt_eval_budget_holds SET remaining_nanos=0 WHERE run_id=?`, id)
	return err
}

// BatchBudget transfers pre-reserved budget into ordinary per-call ledger entries.
type BatchBudget struct {
	Store Store
	RunID string
}

func (b BatchBudget) Reserve(ctx context.Context, a costcontrol.Attempt, limits costcontrol.Limits) error {
	tx, err := b.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lock(ctx, tx); err != nil {
		return err
	}
	var remaining int64
	var month, expires time.Time
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT h.remaining_nanos,h.month_start,h.expires_at,r.status FROM prompt_eval_budget_holds h JOIN prompt_eval_runs r ON r.id=h.run_id WHERE h.run_id=? FOR UPDATE`, b.RunID).Scan(&remaining, &month, &expires, &status); err != nil {
		return err
	}
	if status != "running" || !a.CreatedAt.Before(expires) || !a.MonthStart.Equal(month) || remaining < a.ReservedNanos {
		return costcontrol.ErrBudgetExceeded
	}
	if _, err = tx.ExecContext(ctx, `UPDATE prompt_eval_budget_holds SET remaining_nanos=remaining_nanos-? WHERE run_id=?`, a.ReservedNanos, b.RunID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_cost_months SET charged_nanos=charged_nanos-? WHERE month_start=?`, a.ReservedNanos, month); err != nil {
		return err
	}
	a.Operation = "journal.evaluation." + a.Operation
	if err = mysqlstore.ReserveCostAttempt(ctx, tx, a, limits); err != nil {
		return err
	}
	return tx.Commit()
}
func (b BatchBudget) Settle(ctx context.Context, id string, cost int64, known bool, now time.Time) error {
	return mysqlstore.NewCostControlStore(b.Store.DB).Settle(ctx, id, cost, known, now)
}
func (b BatchBudget) RecordOutcome(ctx context.Context, id string, o costcontrol.Outcome, now time.Time) error {
	return mysqlstore.NewCostControlStore(b.Store.DB).RecordOutcome(ctx, id, o, now)
}

func (s Store) Sample(ctx context.Context, id string, now time.Time) (Sample, error) {
	var value Sample
	var raw, nonce []byte
	err := s.DB.QueryRowContext(ctx, `SELECT payload,nonce FROM prompt_eval_samples WHERE id=? AND expires_at>?`, id, now).Scan(&raw, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, err
	}
	err = s.open(id, raw, nonce, &value)
	return value, err
}
func (s Store) Result(ctx context.Context, id string, index int, now time.Time) (Item, error) {
	var item Item
	var raw, nonce []byte
	err := s.DB.QueryRowContext(ctx, `SELECT i.item_index,i.status,i.payload,i.nonce FROM prompt_eval_items i JOIN prompt_eval_runs r ON r.id=i.run_id WHERE i.run_id=? AND i.item_index=? AND r.expires_at>?`, id, index, now).Scan(&item.Index, &item.Status, &raw, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if len(raw) > 0 {
		item.Result = &ItemResult{}
		if err = s.open(itemKey(id, index), raw, nonce, item.Result); err != nil {
			return item, err
		}
	}
	return item, nil
}

func (s Store) Replay(ctx context.Context, m aiconfig.Mutation, input any, now time.Time) (*Run, error) {
	if !aiconfig.ValidMutation(m) {
		return nil, ErrInvalid
	}
	raw, _ := json.Marshal(input)
	hash := sha256.Sum256(raw)
	var id string
	var old []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id,request_hash FROM prompt_eval_runs WHERE actor_id=? AND idempotency_key=?`, m.Actor, m.Key).Scan(&id, &old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(old, hash[:]) {
		return nil, ErrConflict
	}
	r, err := s.Get(ctx, id, now)
	return &r, err
}
