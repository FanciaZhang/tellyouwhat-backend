package offerdelivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

const reportHeader = "Date\tApp Name\tApp Apple ID\tSubscription Name\tSubscription Apple ID\tOffer Reference Name\tOffer Code\tTerritory\tRedemptions\n"

func reportLine(day, app, subscription, offer, code string, n int) string {
	return fmt.Sprintf("%s\tApp\t%s\tSubscription\t%s\t%s\t%s\tCN\t%d\n", day, app, subscription, offer, code, n)
}
func TestAppleReportParsingRejectsPartialOrAmbiguousInput(t *testing.T) {
	good := reportHeader + reportLine("2026-09-08", "123", "456", "FRIENDS", "PERSONAL123", 1)
	rows, err := ParseAppleReport([]byte(good), "123", "2026-09-08")
	if err != nil || len(rows) != 1 || rows[0].Redemptions != 1 {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Replace(good, "Offer Code", "Unknown", 1), good + reportLine("2026-09-08", "123", "456", "FRIENDS", "PERSONAL123", 1), strings.Replace(good, "2026-09-08", "2026-09-07", 1), strings.Replace(good, "\t1\n", "\t-1\n", 1), strings.Replace(good, "PERSONAL123", "=FORMULA", 1)} {
		if _, err := ParseAppleReport([]byte(raw), "123", "2026-09-08"); !errors.Is(err, ErrInvalid) {
			t.Fatal("accepted invalid/incomplete report")
		}
	}
	rows, err = ParseAppleReport([]byte(good+reportLine("2026-09-08", "999", "456", "FRIENDS", "FOREIGN123", 20)), "123", "2026-09-08")
	if err != nil || len(rows) != 1 {
		t.Fatal("App scoping failed")
	}
}
func TestReportCorrectionKeepsExactCustomCodeAndSubscriptionCounts(t *testing.T) {
	s, p, now := fixture(t)
	ctx := context.Background()
	p.ID = "personal-pool"
	p.Kind = "custom"
	p.SubscriptionID = "456"
	p.ProductID = "app.monthly"
	p.Code = "PERSONAL123"
	p.Capacity = 1
	if err := s.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	raw := []byte(reportHeader + reportLine("2026-09-08", "123", "456", "FRIENDS", "PERSONAL123", 1) + reportLine("2026-09-08", "123", "456", "FRIENDS", "CAMPAIGN123", 7) + reportLine("2026-09-08", "123", "789", "FRIENDS", "PERSONAL123", 4) + reportLine("2026-09-08", "123", "456", "FRIENDS", "", 3) + reportLine("2026-09-08", "999", "456", "FRIENDS", "PERSONAL123", 10))
	before, err := s.PoolReport(ctx, "health", p)
	if err != nil || before.Days != 0 {
		t.Fatalf("fabricated initial report: %+v %v", before, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ImportAppleReport(ctx, "health", "123", "2026-09-08", raw, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.PoolReport(ctx, "health", p)
	if err != nil || got.Redemptions != 1 || got.Days != 1 || got.Scope != "custom_code" {
		t.Fatalf("wrong code total: %+v %v", got, err)
	}
	other := p
	other.Kind = "oneTime"
	got, err = s.PoolReport(ctx, "health", other)
	if err != nil || got.Redemptions != 3 || got.Scope != "one_time_offer" {
		t.Fatalf("one time report inflated: %+v %v", got, err)
	}
	// A correction removes the personal-code row, rather than retaining a stale redemption.
	corrected := []byte(reportHeader + reportLine("2026-09-08", "123", "456", "FRIENDS", "CAMPAIGN123", 8))
	if err = s.ImportAppleReport(ctx, "health", "123", "2026-09-08", corrected, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.PoolReport(ctx, "health", p)
	if err != nil || got.Redemptions != 0 || got.Days != 1 {
		t.Fatalf("correction not applied: %+v %v", got, err)
	}
	if err = s.ImportAppleReport(ctx, "health", "123", "2026-09-06", []byte(reportHeader), now); err != nil {
		t.Fatal(err)
	}
	got, err = s.PoolReport(ctx, "health", p)
	if err != nil || got.Days != 2 || got.FirstDay != "2026-09-06" || got.LastDay != "2026-09-08" {
		t.Fatalf("missing day inferred: %+v %v", got, err)
	}
	if err = s.ImportAppleReport(ctx, "health", "123", "2026-09-08", []byte("truncated"), now); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted invalid correction")
	}
	if err = s.ImportAppleReport(ctx, "health", "999", "2026-09-08", []byte(reportHeader), now); !errors.Is(err, ErrConflict) {
		t.Fatal("accepted a different App identity")
	}
	var n int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM offer_redemption_report_rows WHERE app_id='health' AND report_date='2026-09-08'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("atomic correction failed", err)
	}
}

type fakeReports struct {
	fail  error
	raw   map[string][]byte
	calls []string
}

func (f *fakeReports) ReportsConfigured() bool      { return true }
func (f *fakeReports) OfferScope() (string, string) { return "123", "456" }
func (f *fakeReports) DownloadOfferReport(_ context.Context, day string) ([]byte, error) {
	f.calls = append(f.calls, day)
	if f.fail != nil {
		return nil, f.fail
	}
	raw, ok := f.raw[day]
	if !ok {
		return nil, appstoreconnect.ErrReportNotAvailable
	}
	return raw, nil
}
func TestReportSyncUnavailableNeverBecomesZero(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	source := &fakeReports{raw: map[string][]byte{"2026-09-08": []byte(reportHeader + reportLine("2026-09-08", "123", "456", "FRIENDS", "PERSONAL123", 1))}}
	if err := s.SyncReports(ctx, "health", source, now, 3); err != nil {
		t.Fatal(err)
	}
	var days int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM offer_redemption_report_days WHERE app_id='health'`).Scan(&days); err != nil || days != 1 {
		t.Fatal("missing reports counted as zero", err)
	}
	source.fail = appstoreconnect.ErrForbidden
	if err := s.SyncReports(ctx, "health", source, now, 3); !errors.Is(err, appstoreconnect.ErrForbidden) {
		t.Fatal(err)
	}
	status, err := s.ReportStatus(ctx, "health")
	if err != nil || status.State != "forbidden" {
		t.Fatal("permission failure hidden", err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT SUM(redemptions) FROM offer_redemption_report_rows WHERE app_id='health'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed sync erased evidence", err)
	}
}
