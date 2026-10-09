package illustration

import (
	"context"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"time"
)

// NotDispatched proves this adapter never invoked the upstream generator.
type NotDispatched struct{ Cause error }

func (e NotDispatched) Error() string { return "image request not dispatched" }
func (e NotDispatched) Unwrap() error { return e.Cause }

type BudgetedGenerator struct {
	Next             ImageGenerator
	Controller       *costcontrol.Controller
	MaxCostNanos     int64
	SettlementFailed func()
}

func (g *BudgetedGenerator) Model() string {
	if g == nil || g.Next == nil {
		return ""
	}
	return g.Next.Model()
}
func (g *BudgetedGenerator) Generate(ctx context.Context, input Input) (Result, error) {
	if g == nil || g.Next == nil || g.Controller == nil || g.MaxCostNanos <= 0 {
		return Result{}, NotDispatched{costcontrol.ErrInvalidAttempt}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, NotDispatched{err}
	}
	lease, err := g.Controller.Reserve(ctx, "journal", "journal.illustration", "ark_image", g.MaxCostNanos)
	if err != nil {
		return Result{}, NotDispatched{err}
	}
	settle := func(known bool) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := lease.Settle(cleanup, 0, known); err != nil && g.SettlementFailed != nil {
			g.SettlementFailed()
		}
	}
	if err := ctx.Err(); err != nil {
		settle(true)
		return Result{}, NotDispatched{err}
	}
	result, failure := g.Next.Generate(ctx, input)
	// No authoritative invoice amount is returned. Retain the configured upper
	// bound on success and failure, releasing concurrency but never assuming free.
	settle(false)
	return result, failure
}
