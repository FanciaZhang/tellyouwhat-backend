package offerdelivery

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

type ReportSource interface {
	ReportsConfigured() bool
	OfferScope() (string, string)
	DownloadOfferReport(context.Context, string) ([]byte, error)
}

// SyncReports refreshes recent daily reports to pick up Apple's corrections and
// backfills missing history. Availability failures retain the last good snapshot.
func (s Store) SyncReports(ctx context.Context, app string, source ReportSource, now time.Time, lookback int) error {
	if !source.ReportsConfigured() {
		return ErrUnavailable
	}
	if lookback < 1 || lookback > 185 {
		return ErrInvalid
	}
	appAppleID, _ := source.OfferScope()
	var syncErr error
	for offset := 1; offset <= lookback; offset++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		day := now.UTC().AddDate(0, 0, -offset).Format("2006-01-02")
		exists, err := s.HasReportDay(ctx, app, day)
		if err != nil {
			return err
		}
		if offset > 14 && exists {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		raw, err := source.DownloadOfferReport(call, day)
		cancel()
		if errors.Is(err, appstoreconnect.ErrForbidden) {
			s.recordReportSync(ctx, app, "forbidden", now)
			return err
		}
		if errors.Is(err, appstoreconnect.ErrReportNotAvailable) {
			continue
		}
		if err != nil {
			syncErr = err
			break
		}
		if err = s.ImportAppleReport(ctx, app, appAppleID, day, raw, now); err != nil {
			syncErr = err
			break
		}
	}
	status := "ready"
	if syncErr != nil {
		status = "failed"
	}
	if err := s.recordReportSync(ctx, app, status, now); err != nil {
		return err
	}
	return syncErr
}
func (s Store) recordReportSync(ctx context.Context, app, status string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO offer_redemption_report_sync(app_id,status,attempted_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE status=VALUES(status),attempted_at=VALUES(attempted_at)`, app, status, now.UTC())
	return err
}

type ReportSyncStatus struct {
	State       string     `json:"state"`
	AttemptedAt *time.Time `json:"attemptedAt,omitempty"`
}

func (s Store) ReportStatus(ctx context.Context, app string) (ReportSyncStatus, error) {
	var out ReportSyncStatus
	var attempted time.Time
	err := s.DB.QueryRowContext(ctx, `SELECT status,attempted_at FROM offer_redemption_report_sync WHERE app_id=?`, app).Scan(&out.State, &attempted)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportSyncStatus{State: "pending"}, nil
	}
	if err == nil {
		out.AttemptedAt = &attempted
	}
	return out, err
}
