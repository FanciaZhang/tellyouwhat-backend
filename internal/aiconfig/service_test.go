package aiconfig

import (
	"context"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"testing"
	"time"
)

type testStore struct {
	revision *Revision
	err      error
	reads    int
}

func (s *testStore) Current(context.Context, contracts.Operation) (*Revision, error) {
	s.reads++
	return s.revision, s.err
}
func (*testStore) History(context.Context, contracts.Operation) ([]Revision, error) { panic("unused") }
func (*testStore) Draft(context.Context, Revision) error                            { panic("unused") }
func (*testStore) Publish(context.Context, string, contracts.Operation, string, time.Time) error {
	panic("unused")
}

func TestPublishedPolicyAndRetryIsolation(t *testing.T) {
	s := &testStore{}
	r := Resolver{Store: s}
	original := contracts.Request{Operation: contracts.OperationMealTextCapture, Options: contracts.RequestOptions{ReasoningEffort: "high"}}
	legacy, err := r.Resolve(context.Background(), original)
	if err != nil || legacy.Options.ReasoningEffort != "high" {
		t.Fatal("changed legacy default")
	}
	p := contracts.ExecutionPolicy{Version: "r1", Endpoint: "ep-first", ReasoningEffort: "low", TimeoutSeconds: 90}
	s.revision = &Revision{Policy: p}
	frozen, err := r.Resolve(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Options.ReasoningEffort != "low" {
		t.Fatal("did not apply published setting")
	}
	s.revision.Policy.Endpoint = "ep-new"
	s.err = errors.New("database unavailable")
	retry, err := r.Resolve(context.Background(), frozen)
	if err != nil || retry.ExecutionPolicy.Endpoint != "ep-first" || s.reads != 2 {
		t.Fatal("retry read current policy")
	}
	if _, err = r.Resolve(context.Background(), original); err == nil {
		t.Fatal("silently ignored configuration failure")
	}
}
