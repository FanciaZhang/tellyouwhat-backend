package provider

import (
	"context"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
)

type BudgetedClient struct {
	next       Client
	controller *costcontrol.Controller
	appID      string
	price      costcontrol.TokenPrice
}

func NewBudgetedClient(next Client, controller *costcontrol.Controller, appID string, price costcontrol.TokenPrice) *BudgetedClient {
	return &BudgetedClient{next: next, controller: controller, appID: appID, price: price}
}

func (client *BudgetedClient) Complete(ctx context.Context, request contracts.Request) (Response, error) {
	lease, err := client.reserve(ctx, request)
	if err != nil {
		return Response{}, err
	}
	response, providerErr := client.next.Complete(ctx, request)
	client.settle(ctx, lease, response)
	return response, providerErr
}

func (client *BudgetedClient) Stream(ctx context.Context, request contracts.Request, yield func(StreamEvent) error) error {
	lease, err := client.reserve(ctx, request)
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
	client.settle(ctx, lease, completed)
	return err
}

func (client *BudgetedClient) reserve(ctx context.Context, request contracts.Request) (*costcontrol.Lease, error) {
	if client == nil || client.next == nil || client.controller == nil || !client.price.Valid() {
		return nil, costcontrol.ErrInvalidAttempt
	}
	input := contracts.ReservationTokens(request) - request.OutputTokenReservation()
	reserved, err := client.price.Cost(input, request.OutputTokenReservation())
	if err != nil {
		return nil, err
	}
	return client.controller.Reserve(ctx, client.appID, string(request.Operation), "ark", reserved)
}

func (client *BudgetedClient) settle(ctx context.Context, lease *costcontrol.Lease, response Response) {
	actual, err := client.price.Cost(response.InputTokens, response.OutputTokens)
	_, known := response.KnownTokenTotal()
	if err != nil {
		known = false
		actual = 0
	}
	settlement, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = lease.Settle(settlement, actual, known)
}

func (client *BudgetedClient) CleanupManagedMedia(ctx context.Context, media []contracts.Media) {
	if cleaner, ok := client.next.(ManagedMediaCleaner); ok {
		cleaner.CleanupManagedMedia(ctx, media)
	}
}

var _ Client = (*BudgetedClient)(nil)
var _ ManagedMediaCleaner = (*BudgetedClient)(nil)
