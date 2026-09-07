package airollout

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"regexp"
	"time"
)

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// RecordModel stores operational metadata only. It never infers the responding
// model from the endpoint. Unknown or missing model attribution keeps the full
// reservation rather than treating token usage as a verified cost settlement.
func (s Store) RecordModel(endpoints map[contracts.Operation]string) func(context.Context, contracts.Request, string, costcontrol.TokenPrice) bool {
	return func(ctx context.Context, r contracts.Request, actual string, price costcontrol.TokenPrice) bool {
		id := endpoints[r.Operation]
		version := ""
		if r.ExecutionPolicy != nil {
			id = r.ExecutionPolicy.Endpoint
			version = r.ExecutionPolicy.Version
		}
		if actual != "" && !modelName.MatchString(actual) {
			actual = ""
		}
		raw, _ := json.Marshal(price)
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO health_ai_model_attempts(id,endpoint,operation,policy_version,actual_model,price,created_at) VALUES(?,?,?,?,?,?,?)`, uuid.NewString(), id, r.Operation, version, actual, raw, time.Now().UTC()); err != nil {
			return false
		}
		p, err := s.PriceState(ctx, id)
		if err != nil {
			return false
		}
		if p == nil {
			return actual != ""
		}
		for _, m := range p.Models {
			if actual == m.Name+"-"+m.Version {
				return true
			}
		}
		if actual != "" {
			// Preserve the newest price and model list when marking unexpected traffic.
			_ = s.WithLock(ctx, id, func() error {
				latest, err := s.PriceState(ctx, id)
				if err != nil || latest == nil {
					return err
				}
				latest.Blocked = true
				latest.Drift = true
				return s.SavePrice(ctx, id, *latest)
			})
		}
		return false
	}
}

type ModelAttempt struct {
	Operation     string    `json:"operation"`
	PolicyVersion string    `json:"policyVersion"`
	ActualModel   string    `json:"actualModel"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (s Store) Attempts(ctx context.Context, id string) ([]ModelAttempt, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT operation,policy_version,actual_model,created_at FROM health_ai_model_attempts WHERE endpoint=? ORDER BY created_at DESC LIMIT 20`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelAttempt{}
	for rows.Next() {
		var a ModelAttempt
		if err = rows.Scan(&a.Operation, &a.PolicyVersion, &a.ActualModel, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
