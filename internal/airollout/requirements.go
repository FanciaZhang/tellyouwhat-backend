package airollout

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/contracts"
)

type Requirement struct {
	Operation contracts.Operation `json:"operation"`
	Effort    string              `json:"effort"`
	Search    bool                `json:"search"`
}

func (r Requirement) Policy(endpoint string) contracts.ExecutionPolicy {
	return contracts.ExecutionPolicy{Version: "compatibility", Endpoint: endpoint, ReasoningEffort: r.Effort, WebSearchEnabled: r.Search, TimeoutSeconds: 90}
}

// Requirements includes current bindings and policies that could still be held
// by queued jobs. While jobs remain, historical bindings are conservatively
// retained without decrypting health payloads in the administration process.
func (s *Service) Requirements(ctx context.Context, endpoint string) ([]Requirement, error) {
	required := map[Requirement]bool{}
	add := func(op contracts.Operation, p *contracts.ExecutionPolicy) {
		if p != nil {
			if p.Endpoint == endpoint {
				required[Requirement{op, p.ReasoningEffort, p.WebSearchEnabled}] = true
			}
		} else if s.Endpoints[op] == endpoint {
			for _, effort := range []string{"", "minimal", "low", "medium", "high"} {
				required[Requirement{op, effort, false}] = true
				if op == contracts.OperationMealDecision {
					required[Requirement{op, effort, true}] = true
				}
			}
		}
	}
	for _, op := range contracts.OperationValues() {
		var p *contracts.ExecutionPolicy
		if s.Configurations != nil {
			current, err := s.Configurations.Current(ctx, op)
			if err != nil {
				return nil, err
			}
			if current != nil {
				p = &current.Policy
			}
		}
		add(op, p)
	}
	if s.Store.DB != nil && s.Configurations != nil {
		var queued bool
		err := s.Store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM ai_jobs WHERE app_id='health' AND status IN ('queued','running') AND expires_at > ?)", time.Now().UTC()).Scan(&queued)
		if err != nil {
			return nil, err
		}
		if queued {
			// A policy can be resolved before the job creation timestamp. Retain all
			// historical candidates until queued jobs drain rather than infer which
			// encrypted policy an individual job holds from publication timestamps.
			rows, err := s.Store.DB.QueryContext(ctx, "SELECT document FROM health_ai_config_revisions WHERE published_at IS NOT NULL")
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var raw []byte
				var revision aiconfig.Revision
				if err = rows.Scan(&raw); err == nil {
					err = json.Unmarshal(raw, &revision)
				}
				if err != nil {
					rows.Close()
					return nil, err
				}
				add(revision.Operation, &revision.Policy)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			for _, op := range contracts.OperationValues() {
				add(op, nil)
			}
		}
	}
	out := make([]Requirement, 0, len(required))
	for r := range required {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		if a.Effort != b.Effort {
			return a.Effort < b.Effort
		}
		return !a.Search && b.Search
	})
	return out, nil
}
