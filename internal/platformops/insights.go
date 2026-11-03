package platformops

import (
	"context"
	"database/sql"
	"time"
)

// Segment contains observed provider attempts, including retries and errors.
// EstimatedNanos uses the configured tariff ceiling; it is not an invoice.
type Segment struct {
	Day            string `json:"day"`
	AppID          string `json:"appID"`
	Operation      string `json:"operation"`
	Audience       string `json:"audience"`
	Environment    string `json:"environment"`
	Calls          int64  `json:"calls"`
	UsageKnown     int64  `json:"usageKnown"`
	InputTokens    int64  `json:"inputTokens"`
	OutputTokens   int64  `json:"outputTokens"`
	EstimatedNanos int64  `json:"estimatedNanos"`
	UnpricedCalls  int64  `json:"unpricedCalls"`
}

type Cohort struct {
	AppID        string `json:"appID"`
	Days         int    `json:"days"`
	Observed     int64  `json:"observed"`
	Eligible     int64  `json:"eligible"`
	Matured      int64  `json:"matured"`
	Converted    int64  `json:"converted"`
	Pending      int64  `json:"pending"`
	UnknownPrice int64  `json:"unknownPrice"`
}
type Insights struct {
	CollectionStartedAt time.Time `json:"collectionStartedAt"`
	TrendStart          time.Time `json:"trendStart"`
	CohortStart         time.Time `json:"cohortStart"`
	Segments            []Segment `json:"segments"`
	Cohorts             []Cohort  `json:"cohorts"`
}

func readInsights(ctx context.Context, tx *sql.Tx, now time.Time) (Insights, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	out := Insights{TrendStart: day.AddDate(0, 0, -29), CohortStart: day.AddDate(0, 0, -90), Segments: []Segment{}, Cohorts: []Cohort{}}
	if err := tx.QueryRowContext(ctx, `SELECT started_at FROM operations_collection WHERE singleton_id=1`).Scan(&out.CollectionStartedAt); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DATE_FORMAT(created_at,'%Y-%m-%d'),app_id,operation,audience,environment,COUNT(*),SUM(usage_known),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(CASE WHEN status='settled' THEN actual_nanos ELSE 0 END),0),SUM(status<>'settled') FROM ai_cost_attempts WHERE created_at>=? AND created_at<=? GROUP BY DATE_FORMAT(created_at,'%Y-%m-%d'),app_id,operation,audience,environment ORDER BY 1,2,3,4,5`, out.TrendStart, now)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var s Segment
		if err = rows.Scan(&s.Day, &s.AppID, &s.Operation, &s.Audience, &s.Environment, &s.Calls, &s.UsageKnown, &s.InputTokens, &s.OutputTokens, &s.EstimatedNanos, &s.UnpricedCalls); err != nil {
			rows.Close()
			return out, err
		}
		out.Segments = append(out.Segments, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// A mature cohort uses a complete observation window. Restores of a
	// subscription predating the first free request are not acquisitions.
	// Only one device can claim a given original subscription acquisition.
	for _, days := range []int{7, 30} {
		rows, err = tx.QueryContext(ctx, `WITH candidates AS (
 SELECT c.*,
 EXISTS(SELECT 1 FROM managed_entitlements e WHERE e.app_id=c.app_id AND e.key_id=c.key_id AND e.environment='production' AND e.started_at<c.first_free_at)
 OR EXISTS(SELECT 1 FROM operations_purchase_observations p WHERE p.app_id=c.app_id AND p.key_id=c.key_id AND p.environment='production' AND p.started_at<c.first_free_at) AS returning_subscriber,
 EXISTS(SELECT 1 FROM operations_purchase_observations p WHERE p.app_id=c.app_id AND p.key_id=c.key_id AND p.environment='production' AND p.started_at>=c.first_free_at AND p.purchased_at>=c.first_free_at AND p.purchased_at<=DATE_ADD(c.first_free_at,INTERVAL ? DAY) AND p.purchased_at<=? AND p.price_milli>0 AND p.revoked_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM operations_purchase_observations q JOIN operations_free_cohorts d ON d.app_id=q.app_id AND d.key_id=q.key_id WHERE q.app_id=p.app_id AND q.original_transaction_id=p.original_transaction_id AND q.environment=p.environment AND d.first_free_at<=p.started_at AND (d.first_free_at<c.first_free_at OR (d.first_free_at=c.first_free_at AND d.key_id<c.key_id)))) AS paid,
 EXISTS(SELECT 1 FROM operations_purchase_observations p WHERE p.app_id=c.app_id AND p.key_id=c.key_id AND p.environment='production' AND ((p.purchased_at>=c.first_free_at AND p.purchased_at<=DATE_ADD(c.first_free_at,INTERVAL ? DAY) AND p.purchased_at<=?) OR (p.purchased_at IS NULL AND p.signed_at>=c.first_free_at)) AND (p.price_milli IS NULL OR p.started_at IS NULL OR p.purchased_at IS NULL)) AS price_unknown
 FROM operations_free_cohorts c WHERE c.first_free_at>=? AND c.first_free_at<=?)
 SELECT app_id,COUNT(*),SUM(NOT returning_subscriber),SUM(NOT returning_subscriber AND first_free_at<=?),SUM(NOT returning_subscriber AND first_free_at<=? AND paid),SUM(NOT returning_subscriber AND first_free_at>?),SUM(NOT returning_subscriber AND first_free_at<=? AND price_unknown) FROM candidates GROUP BY app_id`, days, now, days, now, out.CohortStart, now, now.AddDate(0, 0, -days), now.AddDate(0, 0, -days), now.AddDate(0, 0, -days), now.AddDate(0, 0, -days))
		if err != nil {
			return out, err
		}
		for rows.Next() {
			c := Cohort{Days: days}
			if err = rows.Scan(&c.AppID, &c.Observed, &c.Eligible, &c.Matured, &c.Converted, &c.Pending, &c.UnknownPrice); err != nil {
				rows.Close()
				return out, err
			}
			out.Cohorts = append(out.Cohorts, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
