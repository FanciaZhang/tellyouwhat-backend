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

func TestBudgetedProviderFreezesAttemptPriceAndKeepsUnknownModelReservation(t *testing.T) {
	store := &providerBudgetStore{}
	controller, err := costcontrol.New(store, costcontrol.Limits{MonthlyBudgetNanos: 1_000_000_000_000, MaxConcurrent: 2, LeaseDuration: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	first := costcontrol.TokenPrice{InputNanosPerMillionTokens: 12_000_000_000, OutputNanosPerMillionTokens: 8_000_000_000}
	second := costcontrol.TokenPrice{InputNanosPerMillionTokens: 27_000_000_000, OutputNanosPerMillionTokens: 10_800_000_000}
	next := &providerStub{response: Response{InputTokens: 100, OutputTokens: 20, ActualModel: "model-returned"}}
	client := NewBudgetedClient(next, controller, "health", first)
	current := first
	client.ResolvePrice = func(context.Context, contracts.Request) (*costcontrol.TokenPrice, error) {
		copy := current
		return &copy, nil
	}
	client.RecordModel = func(_ context.Context, _ contracts.Request, model string, p costcontrol.TokenPrice) bool {
		if p != first || model != "model-returned" {
			t.Fatal("wrong per-attempt attribution")
		}
		current = second
		return true
	}
	r := contracts.Request{Operation: contracts.OperationMealTextCapture, Prompt: "synthetic"}
	if _, err = client.Complete(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	expected, _ := first.Cost(100, 20)
	if store.actual != expected {
		t.Fatal("settlement read a later price")
	}
	client.RecordModel = func(context.Context, contracts.Request, string, costcontrol.TokenPrice) bool { return false }
	if _, err = client.Complete(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if store.known {
		t.Fatal("unknown model released reservation")
	}
	calls := next.calls
	client.ResolvePrice = func(context.Context, contracts.Request) (*costcontrol.TokenPrice, error) {
		return nil, costcontrol.ErrInvalidAttempt
	}
	if _, err = client.Complete(context.Background(), r); err == nil || next.calls != calls {
		t.Fatal("unavailable pricing reached provider")
	}
}
