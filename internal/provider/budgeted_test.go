package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type providerBudgetStore struct {
	attempt costcontrol.Attempt
	actual  int64
	known   bool
	deny    error
}

func (store *providerBudgetStore) Reserve(_ context.Context, attempt costcontrol.Attempt, _ costcontrol.Limits) error {
	store.attempt = attempt
	return store.deny
}

func (store *providerBudgetStore) Settle(_ context.Context, _ string, actual int64, known bool, _ time.Time) error {
	store.actual, store.known = actual, known
	return nil
}

type providerStub struct {
	calls    int
	response Response
	err      error
}

func (stub *providerStub) Complete(context.Context, contracts.Request) (Response, error) {
	stub.calls++
	return stub.response, stub.err
}

func (stub *providerStub) Stream(_ context.Context, _ contracts.Request, yield func(StreamEvent) error) error {
	stub.calls++
	if stub.response != (Response{}) {
		if err := yield(StreamEvent{Completed: &stub.response}); err != nil {
			return err
		}
	}
	return stub.err
}

func TestBudgetedProviderReservesBeforeCallAndSettlesUsage(t *testing.T) {
	store := &providerBudgetStore{}
	controller, err := costcontrol.New(store, costcontrol.Limits{
		MonthlyBudgetNanos: 1_000_000_000, MaxConcurrent: 2, LeaseDuration: time.Minute,
	}, func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 1_000_000_000, OutputNanosPerMillionTokens: 2_000_000_000}
	next := &providerStub{response: Response{InputTokens: 10, OutputTokens: 20}, err: errors.New("provider failed after metering")}
	client := NewBudgetedClient(next, controller, "health", price)
	request := contracts.Request{Operation: contracts.OperationMealTextCapture, Prompt: "meal", ResponseSchema: []byte(`{"type":"object"}`)}
	if _, err := client.Complete(context.Background(), request); err == nil {
		t.Fatal("provider error was hidden")
	}
	wantReserved, _ := price.Cost(contracts.ReservationTokens(request)-request.OutputTokenReservation(), request.OutputTokenReservation())
	wantActual, _ := price.Cost(10, 20)
	if next.calls != 1 || store.attempt.ReservedNanos != wantReserved || store.attempt.Operation != string(request.Operation) ||
		!store.known || store.actual != wantActual {
		t.Fatalf("calls=%d attempt=%+v actual=%d known=%v", next.calls, store.attempt, store.actual, store.known)
	}

	store.deny = costcontrol.ErrBudgetExceeded
	if _, err := client.Complete(context.Background(), request); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("budget error = %v", err)
	}
	if next.calls != 1 {
		t.Fatal("provider was called after the project budget rejected admission")
	}
}

var _ Client = (*providerStub)(nil)
