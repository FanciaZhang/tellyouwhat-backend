package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

type budgetStore struct {
	reserved costcontrol.Attempt
	actual   int64
	known    bool
	deny     error
}

func (store *budgetStore) Reserve(_ context.Context, attempt costcontrol.Attempt, _ costcontrol.Limits) error {
	store.reserved = attempt
	return store.deny
}

func (store *budgetStore) Settle(_ context.Context, _ string, actual int64, known bool, _ time.Time) error {
	store.actual, store.known = actual, known
	return nil
}

type organizerStub struct {
	calls  int
	result Result
	err    error
}

func (stub *organizerStub) Organize(context.Context, contracts.OrganizeRequest, bool) (Result, error) {
	stub.calls++
	return stub.result, stub.err
}

func TestBudgetedOrganizerReservesBeforeCallAndSettlesMeteredFailure(t *testing.T) {
	store := &budgetStore{}
	controller, err := costcontrol.New(store, costcontrol.Limits{
		MonthlyBudgetNanos: 1_000_000_000, MaxConcurrent: 2, LeaseDuration: time.Minute,
	}, func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 1_000_000_000, OutputNanosPerMillionTokens: 2_000_000_000}
	next := &organizerStub{result: Result{InputTokens: 10, OutputTokens: 20}, err: ErrInvalidResult}
	client := NewBudgetedClient(next, controller, "journal", price)
	request := contracts.OrganizeRequest{Title: "一天", Body: "正文"}
	if _, err := client.Organize(context.Background(), request, true); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("provider error = %v", err)
	}
	wantReserved, _ := price.Cost(contracts.ReservationTokens(request)-contracts.OutputReservationTokens, contracts.OutputReservationTokens)
	wantActual, _ := price.Cost(10, 20)
	if next.calls != 1 || store.reserved.ReservedNanos != wantReserved || store.reserved.Operation != "journal.organize.pro" ||
		!store.known || store.actual != wantActual {
		t.Fatalf("calls=%d attempt=%+v actual=%d known=%v", next.calls, store.reserved, store.actual, store.known)
	}

	store.deny = costcontrol.ErrBudgetExceeded
	if _, err := client.Organize(context.Background(), request, false); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if next.calls != 1 {
		t.Fatal("provider was called after the project budget rejected admission")
	}
}
