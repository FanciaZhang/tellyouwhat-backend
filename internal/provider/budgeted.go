package provider

import (
	"context"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type PriceResolver func(context.Context, contracts.Request) (*costcontrol.TokenPrice, error)

type AtomicReservation func(context.Context, contracts.Request, *costcontrol.Controller, string, costcontrol.TokenPrice) (*costcontrol.Lease, costcontrol.TokenPrice, error)

type ModelRecorder func(context.Context, contracts.Request, string, costcontrol.TokenPrice) bool

type BudgetedClient struct {
	RecordModel    ModelRecorder
	ReserveAttempt AtomicReservation
	ResolvePrice   PriceResolver
	next           Client
	controller     *costcontrol.Controller
	appID          string
	price          costcontrol.TokenPrice
}

func NewBudgetedClient(next Client, controller *costcontrol.Controller, appID string, price costcontrol.TokenPrice) *BudgetedClient {
	return &BudgetedClient{next: next, controller: controller, appID: appID, price: price}
}

func (client *BudgetedClient) Complete(ctx context.Context, request contracts.Request) (Response, error) {
	lease, price, err := client.admit(ctx, request)
	if err != nil {
		return Response{}, err
	}
	response, providerErr := client.next.Complete(ctx, request)
	client.settle(ctx, lease, response, price, client.record(ctx, request, response, price), providerErr == nil)
	return response, providerErr
}

func (client *BudgetedClient) Stream(ctx context.Context, request contracts.Request, yield func(StreamEvent) error) error {
	lease, price, err := client.admit(ctx, request)
	if err != nil {
		return err
	}
	var completed Response
	err = client.next.Stream(ctx, request, func(event StreamEvent) error {
		if event.Completed != nil {
			completed = *event.Completed
		}
		return yield(event)
	})
	client.settle(ctx, lease, completed, price, client.record(ctx, request, completed, price), err == nil)
	return err
}

func (client *BudgetedClient) reserve(ctx context.Context, request contracts.Request, price costcontrol.TokenPrice) (*costcontrol.Lease, error) {
	if client == nil || client.next == nil || client.controller == nil || !price.Valid() {
		return nil, costcontrol.ErrInvalidAttempt
	}
	input := contracts.ReservationTokens(request) - request.OutputTokenReservation()
	reserved, err := price.Cost(input, request.OutputTokenReservation())
	if err != nil {
		return nil, err
	}
	return client.controller.Reserve(ctx, client.appID, string(request.Operation), "ark", reserved)
}

func (client *BudgetedClient) settle(ctx context.Context, lease *costcontrol.Lease, response Response, price costcontrol.TokenPrice, attributed, success bool) {
	actual, err := price.Cost(response.InputTokens, response.OutputTokens)
	_, known := response.KnownTokenTotal()
	if err != nil || !attributed {
		known = false
		actual = 0
	}
	settlement, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = lease.Finish(settlement, actual, known, costcontrol.Outcome{Success: success, InputTokens: max(0, response.InputTokens), OutputTokens: max(0, response.OutputTokens), Model: response.ActualModel})
}

func (client *BudgetedClient) CleanupManagedMedia(ctx context.Context, media []contracts.Media) {
	if cleaner, ok := client.next.(ManagedMediaCleaner); ok {
		cleaner.CleanupManagedMedia(ctx, media)
	}
}

var _ Client = (*BudgetedClient)(nil)
var _ ManagedMediaCleaner = (*BudgetedClient)(nil)

func (client *BudgetedClient) attemptPrice(ctx context.Context, request contracts.Request) (costcontrol.TokenPrice, error) {
	if client == nil {
		return costcontrol.TokenPrice{}, costcontrol.ErrInvalidAttempt
	}
	if client.ResolvePrice != nil {
		p, err := client.ResolvePrice(ctx, request)
		if err != nil {
			return costcontrol.TokenPrice{}, err
		}
		if p != nil {
			return *p, nil
		}
	}
	return client.price, nil
}

func (client *BudgetedClient) admit(ctx context.Context, r contracts.Request) (*costcontrol.Lease, costcontrol.TokenPrice, error) {
	if client == nil || client.next == nil || client.controller == nil {
		return nil, costcontrol.TokenPrice{}, costcontrol.ErrInvalidAttempt
	}
	if client.ReserveAttempt != nil {
		return client.ReserveAttempt(ctx, r, client.controller, client.appID, client.price)
	}
	p, err := client.attemptPrice(ctx, r)
	if err != nil {
		return nil, p, err
	}
	lease, err := client.reserve(ctx, r, p)
	return lease, p, err
}

func (client *BudgetedClient) record(ctx context.Context, r contracts.Request, response Response, p costcontrol.TokenPrice) bool {
	if client.RecordModel == nil {
		return true
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return client.RecordModel(work, r, response.ActualModel, p)
}
