package aiconfig

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"time"
)

var ErrConflict = errors.New("AI configuration version conflict")
var ErrInvalid = errors.New("invalid AI configuration")
var ErrNotFound = errors.New("AI configuration not found")

type Mutation struct {
	Actor     string
	Key       string
	RequestID string
}
type Publication struct {
	Operation   contracts.Operation `json:"operation"`
	Revision    string              `json:"revision"`
	BaseVersion string              `json:"baseVersion"`
}
type HistoryPage struct {
	Revisions  []Revision `json:"revisions"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

type Revision struct {
	ID          string                    `json:"id"`
	Operation   contracts.Operation       `json:"operation"`
	BaseVersion string                    `json:"baseVersion"`
	Policy      contracts.ExecutionPolicy `json:"policy"`
	CreatedBy   string                    `json:"createdBy"`
	CreatedAt   time.Time                 `json:"createdAt"`
	PublishedAt *time.Time                `json:"publishedAt,omitempty"`
}
type CurrentReader interface {
	Current(context.Context, contracts.Operation) (*Revision, error)
}
type Store interface {
	CurrentReader
	History(context.Context, contracts.Operation) ([]Revision, error)
	HistoryPage(context.Context, contracts.Operation, string) (HistoryPage, error)
	Get(context.Context, contracts.Operation, string) (*Revision, error)
	Replay(context.Context, Mutation, string, any) (*Revision, error)
	Draft(context.Context, Revision, Mutation) (Revision, error)
	Publish(context.Context, Publication, Mutation, time.Time) (Revision, error)
}

// Resolver applies only explicitly published revisions. Legacy defaults remain
// client-owned until the first publication for an operation.
type Resolver struct{ Store CurrentReader }

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
