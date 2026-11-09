package offerdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaximumReportBytes = 8 << 20

var reportCodePattern = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)

type ReportRow struct {
	SubscriptionID string
	OfferName      string
	CodeHash       [32]byte
	Territory      string
	Redemptions    int
}

// ParseAppleReport accepts a complete daily TSV. Only the configured App's rows
// are retained; one-time code values remain absent exactly as Apple reports them.
func ParseAppleReport(raw []byte, appAppleID, day string) ([]ReportRow, error) {
	if len(raw) > MaximumReportBytes || len(raw) == 0 || !decimalID(appAppleID) {
		return nil, ErrInvalid
	}
	target, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil, ErrInvalid
	}
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	reader.Comma = '\t'
	header, err := reader.Read()
	if err != nil {
		return nil, ErrInvalid
	}
	columns := map[string]int{}
	for i, v := range header {
		if _, exists := columns[v]; exists {
			return nil, ErrInvalid
		}
		columns[v] = i
	}
	for _, field := range []string{"Date", "App Name", "App Apple ID", "Subscription Name", "Subscription Apple ID", "Offer Reference Name", "Offer Code", "Territory", "Redemptions"} {
		if _, ok := columns[field]; !ok {
			return nil, ErrInvalid
		}
	}
	rows := []ReportRow{}
	seen := map[string]bool{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrInvalid
		}
		value := func(field string) string { return row[columns[field]] }
		date, err := time.Parse("2006-01-02", value("Date"))
		if err != nil {
			date, err = time.Parse("01/02/2006", value("Date"))
		}
		if err != nil || !date.Equal(target) {
			return nil, ErrInvalid
		}
		if value("App Apple ID") != appAppleID {
			continue
		}
		subscription, offer, code, territory := value("Subscription Apple ID"), value("Offer Reference Name"), value("Offer Code"), value("Territory")
		count, err := strconv.Atoi(value("Redemptions"))
		if err != nil || count < 0 || count > 1000000 || !decimalID(subscription) || offer == "" || len(offer) > 1000 || (code != "" && !reportCodePattern.MatchString(code)) || len(territory) != 2 || territory[0] < 'A' || territory[0] > 'Z' || territory[1] < 'A' || territory[1] > 'Z' {
			return nil, ErrInvalid
		}
		key := strings.Join([]string{subscription, offer, code, territory}, "\x00")
		if seen[key] {
			return nil, ErrInvalid
		}
		seen[key] = true
		rows = append(rows, ReportRow{SubscriptionID: subscription, OfferName: offer, CodeHash: sha256.Sum256([]byte(code)), Territory: territory, Redemptions: count})
	}
	return rows, nil
}
func decimalID(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ImportAppleReport is used only for successfully downloaded Apple API reports.
// A correction replaces the entire App/day snapshot atomically, including rows
// removed by Apple. A failed or missing download must never call this method.
func (s Store) ImportAppleReport(ctx context.Context, app, appAppleID, day string, raw []byte, now time.Time) error {
	if !validApp(app) {
		return ErrInvalid
	}
	rows, err := ParseAppleReport(raw, appAppleID, day)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO offer_redemption_report_days(app_id,report_date,app_apple_id,digest,fetched_at) VALUES(?,?,?,?,?)`, app, day, appAppleID, make([]byte, 32), now.UTC()); err != nil {
		return err
	}
	var oldDigest []byte
	var oldApp string
	if err = tx.QueryRowContext(ctx, `SELECT digest,app_apple_id FROM offer_redemption_report_days WHERE app_id=? AND report_date=? FOR UPDATE`, app, day).Scan(&oldDigest, &oldApp); err != nil {
		return err
	}
	if oldApp != appAppleID {
		return ErrConflict
	}
	if !bytes.Equal(oldDigest, digest[:]) {
		if _, err = tx.ExecContext(ctx, `DELETE FROM offer_redemption_report_rows WHERE app_id=? AND report_date=?`, app, day); err != nil {
			return err
		}
		for _, row := range rows {
			if _, err = tx.ExecContext(ctx, `INSERT INTO offer_redemption_report_rows(app_id,report_date,subscription_id,offer_name,code_hash,territory,redemptions) VALUES(?,?,?,?,?,?,?)`, app, day, row.SubscriptionID, row.OfferName, row.CodeHash[:], row.Territory, row.Redemptions); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_redemption_report_days SET digest=?,fetched_at=? WHERE app_id=? AND report_date=?`, digest[:], now.UTC(), app, day); err != nil {
		return err
	}
	return tx.Commit()
}

type ReportSummary struct {
	Redemptions int        `json:"redemptions"`
	Days        int        `json:"days"`
	FirstDay    string     `json:"firstDay"`
	LastDay     string     `json:"lastDay"`
	FetchedAt   *time.Time `json:"fetchedAt,omitempty"`
	Scope       string     `json:"scope"`
}

func (s Store) PoolReport(ctx context.Context, app string, p Pool) (ReportSummary, error) {
	out := ReportSummary{Scope: "one_time_offer"}
	code := ""
	if p.Environment != "production" || p.SubscriptionID == "" {
		out.Scope = "unavailable"
		return out, nil
	}
	if p.Kind == "custom" {
		out.Scope = "custom_code"
		var encrypted, nonce []byte
		if err := s.DB.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM offer_delivery_pools WHERE app_id=? AND pool_id=?`, app, p.ID).Scan(&encrypted, &nonce); err != nil {
			return out, err
		}
		raw, err := s.Cipher.Decrypt(encrypted, nonce, aad(app, "pool", p.ID))
		if err != nil {
			return out, err
		}
		code = string(raw)
	}
	hash := sha256.Sum256([]byte(code))
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var first, last sql.NullTime
	var fetched sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),MIN(report_date),MAX(report_date),MAX(fetched_at) FROM offer_redemption_report_days WHERE app_id=?`, app).Scan(&out.Days, &first, &last, &fetched); err != nil {
		return out, err
	}
	if first.Valid {
		out.FirstDay = first.Time.Format("2006-01-02")
		out.LastDay = last.Time.Format("2006-01-02")
		out.FetchedAt = &fetched.Time
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(redemptions),0) FROM offer_redemption_report_rows WHERE app_id=? AND subscription_id=? AND BINARY offer_name=BINARY ? AND code_hash=?`, app, p.SubscriptionID, p.OfferName, hash[:]).Scan(&out.Redemptions); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s Store) HasReportDay(ctx context.Context, app, day string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM offer_redemption_report_days WHERE app_id=? AND report_date=?`, app, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
