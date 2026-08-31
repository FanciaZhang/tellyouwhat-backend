package adminportal

import (
	"context"
	"database/sql"
	"time"
)

type OfferMetric struct {
	OfferIdentifier string    `json:"offerIdentifier"`
	Environment     string    `json:"environment"`
	Redemptions     int       `json:"redemptions"`
	UniqueAccounts  int       `json:"uniqueAccounts"`
	LastRedeemedAt  time.Time `json:"lastRedeemedAt"`
}

type MetricsReader interface {
	OfferMetrics(context.Context) ([]OfferMetric, error)
}

type MySQLMetricsReader struct{ database *sql.DB }

func NewMySQLMetricsReader(database *sql.DB) *MySQLMetricsReader {
	return &MySQLMetricsReader{database: database}
}

func (reader *MySQLMetricsReader) OfferMetrics(ctx context.Context) ([]OfferMetric, error) {
	rows, err := reader.database.QueryContext(ctx, `
		SELECT offer_identifier, environment, COUNT(*), COUNT(DISTINCT original_transaction_hash), MAX(redeemed_at)
		FROM app_store_offer_redemptions
		GROUP BY offer_identifier, environment
		ORDER BY MAX(redeemed_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metrics []OfferMetric
	for rows.Next() {
		var metric OfferMetric
		if err := rows.Scan(&metric.OfferIdentifier, &metric.Environment, &metric.Redemptions, &metric.UniqueAccounts, &metric.LastRedeemedAt); err != nil {
			return nil, err
		}
		metrics = append(metrics, metric)
	}
	return metrics, rows.Err()
}

var _ MetricsReader = (*MySQLMetricsReader)(nil)
