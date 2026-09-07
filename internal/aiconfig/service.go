package aiconfig

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"time"
)

var ErrConflict = errors.New("AI configuration version conflict")
var ErrInvalid = errors.New("invalid AI configuration")

type Revision struct {
	ID          string                    `json:"id"`
	Operation   contracts.Operation       `json:"operation"`
	BaseVersion string                    `json:"baseVersion"`
	Policy      contracts.ExecutionPolicy `json:"policy"`
	CreatedBy   string                    `json:"createdBy"`
	CreatedAt   time.Time                 `json:"createdAt"`
	PublishedAt *time.Time                `json:"publishedAt,omitempty"`
}
type Store interface {
	Current(context.Context, contracts.Operation) (*Revision, error)
	History(context.Context, contracts.Operation) ([]Revision, error)
	Draft(context.Context, Revision) error
	Publish(context.Context, string, contracts.Operation, string, time.Time) error
}

// Resolver applies only explicitly published revisions. Legacy defaults remain
// client-owned until the first publication for an operation.
type Resolver struct{ Store Store }

func (r Resolver) Resolve(ctx context.Context, request contracts.Request) (contracts.Request, error) {
	if request.ExecutionPolicy != nil {
		return request.WithExecutionPolicy(*request.ExecutionPolicy)
	}
	revision, err := r.Store.Current(ctx, request.Operation)
	if err != nil {
		return contracts.Request{}, err
	}
	if revision == nil {
		return request, nil
	}
	return request.WithExecutionPolicy(revision.Policy)
}
