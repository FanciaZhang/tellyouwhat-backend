package voice

import (
	"context"

	"math"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

const voiceRewriteOutputReservationTokens = 12_000

type BudgetedRewriter struct {
	next       Rewriter
	controller *costcontrol.Controller
	appID      string
	price      costcontrol.TokenPrice
}

func NewBudgetedRewriter(next Rewriter, controller *costcontrol.Controller, appID string, price costcontrol.TokenPrice) *BudgetedRewriter {
	return &BudgetedRewriter{next: next, controller: controller, appID: appID, price: price}
}

func (rewriter *BudgetedRewriter) Rewrite(ctx context.Context, snapshot Snapshot, transcriptRevision int) (RewriteResult, error) {
	if rewriter == nil || rewriter.next == nil || rewriter.controller == nil || !rewriter.price.Valid() {
		return RewriteResult{}, costcontrol.ErrInvalidAttempt
	}
	if err := snapshot.Validate(); err != nil {
		return RewriteResult{}, err
	}
	prepared, err := PrepareRewrite(ctx, snapshot, transcriptRevision, "")
	if err != nil {
		return RewriteResult{}, err
	}
	price := rewriter.price
	if prepared.Parameters.Price != nil {
		price = *prepared.Parameters.Price
	}
	inputReservation := len(prepared.Body) + 1024
	reserved, err := price.Cost(inputReservation, prepared.Parameters.MaxOutputTokens)
	if err != nil {
		return RewriteResult{}, err
	}
	lease, err := rewriter.controller.Reserve(ctx, rewriter.appID, "journal.voice.rewrite", "ark", reserved)
	if err != nil {
		return RewriteResult{}, err
	}
	result, providerErr := rewriter.next.Rewrite(ctx, snapshot, transcriptRevision)
	actual, costErr := price.Cost(result.InputTokens, result.OutputTokens)
	_, known := knownRewriteTokenTotal(result)
	if costErr != nil {
		actual = 0
		known = false
	}
	settleCost(ctx, lease, actual, known, costcontrol.Outcome{Model: result.Model, Success: providerErr == nil, UsageKnown: result.InputTokens >= 0 && result.OutputTokens >= 0 && (result.InputTokens > 0 || result.OutputTokens > 0), InputTokens: max(0, result.InputTokens), OutputTokens: max(0, result.OutputTokens)})
	return result, providerErr
}

func knownRewriteTokenTotal(result RewriteResult) (int, bool) {
	if result.InputTokens < 0 || result.OutputTokens < 0 || result.InputTokens > math.MaxInt-result.OutputTokens {
		return 0, false
	}
	total := result.InputTokens + result.OutputTokens
	return total, total > 0
}

type BudgetedSpeech struct {
	next       Speech
	controller *costcontrol.Controller
	appID      string
	price      costcontrol.DurationPrice
}

func NewBudgetedSpeech(next Speech, controller *costcontrol.Controller, appID string, price costcontrol.DurationPrice) *BudgetedSpeech {
	return &BudgetedSpeech{next: next, controller: controller, appID: appID, price: price}
}

func (speech *BudgetedSpeech) Open(ctx context.Context, words []string) (SpeechConnection, error) {
	if speech == nil || speech.next == nil || speech.controller == nil || speech.price.NanosPerHour <= 0 {
		return nil, costcontrol.ErrInvalidAttempt
	}
	reserved, err := speech.price.Cost(MaxSegmentBytes / 32)
	if err != nil {
		return nil, err
	}
	lease, err := speech.controller.Reserve(ctx, speech.appID, "journal.voice.speech", "speech", reserved)
	if err != nil {
		return nil, err
	}
	connection, providerErr := speech.next.Open(ctx, words)
	if providerErr != nil {
		settleCost(ctx, lease, 0, false, costcontrol.Outcome{})
		return nil, providerErr
	}
	return &budgetedSpeechConnection{next: connection, lease: lease, price: speech.price, parentContext: ctx}, nil
}

type budgetedSpeechConnection struct {
	mu            sync.Mutex
	next          SpeechConnection
	lease         *costcontrol.Lease
	price         costcontrol.DurationPrice
	parentContext context.Context
	bytesSent     int
	uncertain     bool
	closed        bool
}

func (connection *budgetedSpeechConnection) Send(pcm []byte, final bool) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closed || len(pcm) > MaxSegmentBytes-connection.bytesSent {
		return ErrInvalid
	}
	if err := connection.next.Send(pcm, final); err != nil {
		connection.uncertain = true
		return err
	}
	connection.bytesSent += len(pcm)
	return nil
}

func (connection *budgetedSpeechConnection) Receive() (Transcript, error) {
	value, err := connection.next.Receive()
	if err != nil {
		connection.mu.Lock()
		connection.uncertain = true
		connection.mu.Unlock()
	}
	return value, err
}

func (connection *budgetedSpeechConnection) Close() error {
	connection.mu.Lock()
	if connection.closed {
		connection.mu.Unlock()
		return nil
	}
	connection.closed = true
	bytesSent, uncertain := connection.bytesSent, connection.uncertain
	connection.mu.Unlock()
	providerErr := connection.next.Close()
	actual, costErr := connection.price.Cost((bytesSent + 31) / 32)
	known := !uncertain && costErr == nil
	if costErr != nil {
		actual = 0
	}
	settleCost(connection.parentContext, connection.lease, actual, known, costcontrol.Outcome{Success: !uncertain && providerErr == nil})
	return providerErr
}

func settleCost(parent context.Context, lease *costcontrol.Lease, actual int64, known bool, outcome costcontrol.Outcome) {
	settlement, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	_ = lease.Finish(settlement, actual, known, outcome)
}

var _ Rewriter = (*BudgetedRewriter)(nil)
var _ Speech = (*BudgetedSpeech)(nil)
var _ SpeechConnection = (*budgetedSpeechConnection)(nil)
