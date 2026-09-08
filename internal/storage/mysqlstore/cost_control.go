package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
)

type CostControlStore struct{ database *sql.DB }

func NewCostControlStore(database *sql.DB) *CostControlStore {
	return &CostControlStore{database: database}
}

func (store *CostControlStore) Reserve(ctx context.Context, attempt costcontrol.Attempt, limits costcontrol.Limits) error {
	if store == nil || store.database == nil || !attempt.Valid() || !limits.Valid() {
		return costcontrol.ErrInvalidAttempt
	}
	tx, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ReserveCostAttempt(ctx, tx, attempt, limits); err != nil {
		return err
	}
	return tx.Commit()
}

// ReserveCostAttempt shares the project budget lock with durable administrative job admission.
func ReserveCostAttempt(ctx context.Context, tx *sql.Tx, attempt costcontrol.Attempt, limits costcontrol.Limits) error {
	if !attempt.Valid() || !limits.Valid() {
		return costcontrol.ErrInvalidAttempt
	}
	var err error
	if err = lockCostControl(ctx, tx); err != nil {
		return err
	}
	current, policyErr := platformops.CurrentFrom(ctx, tx)
	if policyErr != nil && !errors.Is(policyErr, platformops.ErrNotFound) {
		return policyErr
	}
	if policyErr == nil {
		if err = platformops.ReserveAutomation(ctx, tx, current.Policy, attempt); err != nil {
			return err
		}
		limits.MonthlyBudgetNanos = current.Policy.MonthlyBudgetNanos
		limits.MaxConcurrent = current.Policy.MaxConcurrent
		app, ok := current.Policy.Apps[attempt.AppID]
		if !ok {
			return costcontrol.ErrInvalidAttempt
		}
		if app.MonthlyBudgetNanos > 0 {
			var used int64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN status='settled' THEN actual_nanos ELSE reserved_nanos END),0) FROM ai_cost_attempts WHERE app_id=? AND month_start=?`, attempt.AppID, attempt.MonthStart.UTC().Format("2006-01-02")).Scan(&used); err != nil {
				return err
			}
			var held int64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(remaining_nanos),0) FROM prompt_eval_budget_holds WHERE app_id=? AND month_start=?`, attempt.AppID, attempt.MonthStart.UTC().Format("2006-01-02")).Scan(&held); err != nil {
				return err
			}
			if held > math.MaxInt64-used {
				return costcontrol.ErrInvalidAttempt
			}
			used += held
			if used > app.MonthlyBudgetNanos || attempt.ReservedNanos > app.MonthlyBudgetNanos-used {
				return costcontrol.ErrBudgetExceeded
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE ai_cost_attempts
		SET status = 'unknown', completed_at = ?
		WHERE status = 'pending' AND lease_expires_at <= ?`, attempt.CreatedAt, attempt.CreatedAt); err != nil {
		return err
	}
	month := attempt.MonthStart.UTC().Format("2006-01-02")
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO ai_cost_months (month_start, budget_nanos, charged_nanos, updated_at)
		VALUES (?, ?, 0, ?)
		ON DUPLICATE KEY UPDATE updated_at = VALUES(updated_at)`,
		month, limits.MonthlyBudgetNanos, attempt.CreatedAt); err != nil {
		return err
	}
	var budget, charged int64
	if err = tx.QueryRowContext(ctx, `SELECT budget_nanos, charged_nanos FROM ai_cost_months WHERE month_start = ? FOR UPDATE`, month).Scan(&budget, &charged); err != nil {
		return err
	}
	if budget != limits.MonthlyBudgetNanos {
		if policyErr != nil {
			return costcontrol.ErrConfigurationConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ai_cost_months SET budget_nanos=? WHERE month_start=?`, limits.MonthlyBudgetNanos, month); err != nil {
			return err
		}
	}
	if charged < 0 || charged > limits.MonthlyBudgetNanos || attempt.ReservedNanos > limits.MonthlyBudgetNanos-charged {
		return costcontrol.ErrBudgetExceeded
	}
	var concurrent int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_cost_attempts WHERE status = 'pending'`).Scan(&concurrent); err != nil {
		return err
	}
	if concurrent >= limits.MaxConcurrent && attempt.Operation != "journal.evaluation.reserve" {
		return costcontrol.ErrConcurrencyExceeded
	}
	audience, environment, err := costAudience(ctx, tx, attempt.AppID, attempt.CreatedAt)
	if err != nil {
		return err
	}
	if strings.HasPrefix(attempt.Operation, "journal.evaluation.") {
		audience, environment = "admin_evaluation", "management"
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO ai_cost_attempts
			(id, month_start, app_id, operation, meter, reserved_nanos, status, created_at, lease_expires_at, audience, environment)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		attempt.ID, month, attempt.AppID, attempt.Operation, attempt.Meter,
		attempt.ReservedNanos, attempt.CreatedAt, attempt.LeaseExpiresAt, audience, environment); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_cost_months SET charged_nanos = ?, updated_at = ? WHERE month_start = ?`,
		charged+attempt.ReservedNanos, attempt.CreatedAt, month); err != nil {
		return err
	}
	return nil
}

func (store *CostControlStore) Settle(ctx context.Context, attemptID string, actualNanos int64, known bool, now time.Time) error {
	if store == nil || store.database == nil || attemptID == "" || actualNanos < 0 || now.IsZero() {
		return costcontrol.ErrInvalidAttempt
	}
	tx, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockCostControl(ctx, tx); err != nil {
		return err
	}
	var month, status string
	var reserved int64
	if err = tx.QueryRowContext(ctx, `
		SELECT DATE_FORMAT(month_start, '%Y-%m-%d'), reserved_nanos, status
		FROM ai_cost_attempts WHERE id = ? FOR UPDATE`, attemptID).Scan(&month, &reserved, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return costcontrol.ErrInvalidAttempt
		}
		return err
	}
	if status != "pending" {
		return tx.Commit()
	}
	if !known {
		if _, err = tx.ExecContext(ctx, `
			UPDATE ai_cost_attempts SET status = 'unknown', completed_at = ? WHERE id = ?`, now, attemptID); err != nil {
			return err
		}
		return tx.Commit()
	}
	var charged int64
	if err = tx.QueryRowContext(ctx, `SELECT charged_nanos FROM ai_cost_months WHERE month_start = ? FOR UPDATE`, month).Scan(&charged); err != nil {
		return err
	}
	if charged < reserved || actualNanos > math.MaxInt64-(charged-reserved) {
		return costcontrol.ErrInvalidAttempt
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_cost_months SET charged_nanos = ?, updated_at = ? WHERE month_start = ?`,
		charged-reserved+actualNanos, now, month); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE ai_cost_attempts
		SET status = 'settled', actual_nanos = ?, completed_at = ? WHERE id = ?`, actualNanos, now, attemptID); err != nil {
		return err
	}
	return tx.Commit()
}

func lockCostControl(ctx context.Context, tx *sql.Tx) error {
	var singleton int
	return tx.QueryRowContext(ctx, `SELECT singleton_id FROM ai_cost_control_state WHERE singleton_id = 1 FOR UPDATE`).Scan(&singleton)
}

var _ costcontrol.Store = (*CostControlStore)(nil)
