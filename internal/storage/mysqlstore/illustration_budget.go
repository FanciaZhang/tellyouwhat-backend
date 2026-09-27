package mysqlstore

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/illustration"
)

// ImageAllowancePolicy resolves the authenticated owner's stable billing account
// and server-side monthly image limit. Neither value comes from client JSON.
// No default quota or subscription pricing is invented by this package.
type ImageAllowancePolicy interface {
	ImageAllowance(context.Context, string, time.Time) (account string, monthlyLimit int64, err error)
}

func (r *IllustrationRepository) reserveImage(ctx context.Context, tx *sql.Tx, task illustration.Task, limit int64) error {
	if limit < 1 || strings.TrimSpace(task.BillingAccount) == "" || len(task.BillingAccount) > 255 {
		return illustration.ErrImageBudget
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO journal_image_budgets(app_id,account_id,month) VALUES(?,?,?) ON DUPLICATE KEY UPDATE account_id=account_id`, r.appID, task.BillingAccount, task.BudgetMonth)
	if err != nil {
		return err
	}
	count, err := affectedRows(tx.ExecContext(ctx, `UPDATE journal_image_budgets SET reserved=reserved+1 WHERE app_id=? AND account_id=? AND month=? AND used+reserved<?`, r.appID, task.BillingAccount, task.BudgetMonth, limit))
	if err != nil {
		return err
	}
	if count != 1 {
		return illustration.ErrImageBudget
	}
	return nil
}

func (r *IllustrationRepository) settleImage(ctx context.Context, tx *sql.Tx, before, after illustration.Task) error {
	if before.Charge == after.Charge || (after.Charge != illustration.ChargeRelease && after.Charge != illustration.ChargeOneImage) {
		return nil
	}
	if before.Charge != illustration.ChargeReserved && before.Charge != illustration.ChargeUncertain {
		return illustration.ErrTaskConflict
	}
	used := 0
	if after.Charge == illustration.ChargeOneImage {
		used = 1
	}
	count, err := affectedRows(tx.ExecContext(ctx, `UPDATE journal_image_budgets SET reserved=reserved-1,used=used+? WHERE app_id=? AND account_id=? AND month=? AND reserved>0`, used, r.appID, before.BillingAccount, before.BudgetMonth))
	if err != nil {
		return err
	}
	if count != 1 {
		return illustration.ErrImageBudget
	}
	return nil
}
