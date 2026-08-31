package service

import (
	"context"
	"testing"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/provider"
)

type fake struct {
	calls  []bool
	errors []error
}

func (f *fake) Organize(_ context.Context, _ contracts.OrganizeRequest, pro bool) (provider.Result, error) {
	f.calls = append(f.calls, pro)
	i := len(f.calls) - 1
	if i < len(f.errors) {
		return provider.Result{}, f.errors[i]
	}
	return provider.Result{}, nil
}
func TestLiteFailureRetriesPro(t *testing.T) {
	m := &fake{errors: []error{provider.ErrInvalidResult}}
	s := Organizer{Model: m, LiteMaxCharacters: 2000, LiteMaxBooks: 20, LiteMaxTags: 100}
	_, _ = s.Organize(context.Background(), contracts.OrganizeRequest{Body: "短文"})
	if len(m.calls) != 2 || m.calls[0] || !m.calls[1] {
		t.Fatalf("calls=%v", m.calls)
	}
}
func TestLongBodyStartsPro(t *testing.T) {
	m := &fake{}
	s := Organizer{Model: m, LiteMaxCharacters: 2, LiteMaxBooks: 20, LiteMaxTags: 100}
	_, _ = s.Organize(context.Background(), contracts.OrganizeRequest{Body: "已超过"})
	if len(m.calls) != 1 || !m.calls[0] {
		t.Fatalf("calls=%v", m.calls)
	}
}
