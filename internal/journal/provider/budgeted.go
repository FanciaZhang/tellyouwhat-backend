package provider

import (
	"context"
	"math"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

type Organizer interface {
	Organize(context.Context, contracts.OrganizeRequest, bool) (Result, error)
}

type BudgetedClient struct {
	next       Organizer
	controller *costcontrol.Controller
	appID      string
	price      costcontrol.TokenPrice
}

func NewBudgetedClient(next Organizer, controller *costcontrol.Controller, appID string, price costcontrol.TokenPrice) *BudgetedClient {
	return &BudgetedClient{next: next, controller: controller, appID: appID, price: price}
}

func (client *BudgetedClient) Organize(ctx context.Context, request contracts.OrganizeRequest, pro bool) (Result, error) {
	if client == nil || client.next == nil || client.controller == nil || !client.price.Valid() {
		return Result{}, costcontrol.ErrInvalidAttempt
	}
	input := contracts.ReservationTokens(request) - contracts.OutputReservationTokens
	reserved, err := client.price.Cost(input, contracts.OutputReservationTokens)
	if err != nil {
		return Result{}, err
	}
	operation := "journal.organize.lite"
	if pro {
		operation = "journal.organize.pro"
	}
	lease, err := client.controller.Reserve(ctx, client.appID, operation, "ark", reserved)
	if err != nil {
		return Result{}, err
	}
	result, providerErr := client.next.Organize(ctx, request, pro)
	actual, costErr := client.price.Cost(result.InputTokens, result.OutputTokens)
	_, known := result.KnownTokenTotal()
	if costErr != nil {
		actual = 0
		known = false
	}
	settlement, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = lease.Finish(settlement, actual, known, costcontrol.Outcome{Success: providerErr == nil, InputTokens: max(0, result.InputTokens), OutputTokens: max(0, result.OutputTokens)})
	return result, providerErr
}

func (result Result) KnownTokenTotal() (int, bool) {
	if result.InputTokens < 0 || result.OutputTokens < 0 || result.InputTokens > math.MaxInt-result.OutputTokens {
		return 0, false
	}
	total := result.InputTokens + result.OutputTokens
	return total, total > 0
}

var _ Organizer = (*BudgetedClient)(nil)
