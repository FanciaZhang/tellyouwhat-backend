package mysqlstore

import (
	"context"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

var _ ImageAllowancePolicy = (*illustration.AccessPolicy)(nil)
var _ illustration.ReservationReader = (*IllustrationRepository)(nil)

func (r *IllustrationRepository) HasImageReservation(ctx context.Context, expected illustration.Task) (bool, error) {
	if expected.AppID != r.appID {
		return false, nil
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stored, _, _, err := r.lock(ctx, tx, expected.OwnerID, expected.ID)
	if err != nil {
		return false, err
	}
	if !stored.SameRequest(expected) || stored.BillingAccount != expected.BillingAccount || stored.BudgetMonth != expected.BudgetMonth || stored.Revision != expected.Revision ||
		(stored.State != illustration.TaskQueued && stored.State != illustration.TaskRunning) ||
		(stored.Charge != illustration.ChargeReserved && stored.Charge != illustration.ChargeUncertain) {
		return false, nil
	}
	var reserved bool
	err = tx.QueryRowContext(ctx, `SELECT reserved>0 FROM journal_image_budgets WHERE app_id=? AND account_id=? AND month=?`, r.appID, stored.BillingAccount, stored.BudgetMonth).Scan(&reserved)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return reserved, nil
}
