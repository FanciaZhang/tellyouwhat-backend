package voice

import (
	"context"
	"errors"
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
	settleCost(ctx, lease, actual, known, costcontrol.Outcome{Model: result.Model, Cancelled: costcontrol.IsCancellation(ctx, providerErr), Success: providerErr == nil, UsageKnown: result.InputTokens >= 0 && result.OutputTokens >= 0 && (result.InputTokens > 0 || result.OutputTokens > 0), InputTokens: max(0, result.InputTokens), OutputTokens: max(0, result.OutputTokens)})
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
		settleCost(ctx, lease, 0, false, costcontrol.Outcome{Cancelled: costcontrol.IsCancellation(ctx, providerErr)})
		return nil, providerErr
	}
	return &budgetedSpeechConnection{next: connection, lease: lease, price: speech.price,
		controller: speech.controller, appID: speech.appID, parentContext: ctx}, nil
}

type budgetedSpeechConnection struct {
	mu            sync.Mutex
	next          SpeechConnection
	lease         *costcontrol.Lease
	price         costcontrol.DurationPrice
	controller    *costcontrol.Controller
	appID         string
	parentContext context.Context
	windowBytes   int
	totalBytes    int
	uncertain     bool
	closed        bool
	inputFinal    bool
	failure       error
}

func (connection *budgetedSpeechConnection) Send(pcm []byte, final bool) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closed || connection.inputFinal || len(pcm)%2 != 0 || len(pcm) > SessionMilliseconds*32-connection.totalBytes {
		return ErrInvalid
	}
	if connection.failure != nil {
		return connection.failure
	}
	// Billing checkpoints do not close the acoustic connection. Settle the
	// preceding lease before reserving the next, so ASR + one rewrite still
	// fit the shared two-attempt concurrency limit.
	for offset := 0; offset < len(pcm) || (len(pcm) == 0 && offset == 0); {
		if len(pcm) > offset && connection.windowBytes == MaxSegmentBytes {
			if err := connection.finishWindow(true); err != nil {
				connection.failure = err
				return err
			}
			reserved, err := connection.price.Cost(MaxSegmentBytes / 32)
			if err != nil {
				connection.failure = err
				return err
			}
			lease, err := connection.controller.Reserve(connection.parentContext, connection.appID, "journal.voice.speech", "speech", reserved)
			if err != nil {
				connection.failure = err
				return err
			}
			connection.lease, connection.windowBytes = lease, 0
		}
		count := min(len(pcm)-offset, MaxSegmentBytes-connection.windowBytes)
		last := offset+count == len(pcm)
		if err := connection.next.Send(pcm[offset:offset+count], final && last); err != nil {
			connection.uncertain = true
			connection.failure = err
			return err
		}
		connection.windowBytes += count
		connection.totalBytes += count
		offset += count
		if last {
			connection.inputFinal = final
			break
		}
	}
	return nil
}

// Caller holds mu. Failed settlement leaves the lease available to Close for
// a bounded retry; no more provider audio is sent without budget coverage.
func (connection *budgetedSpeechConnection) finishWindow(success bool) error {
	if connection.lease == nil {
		return nil
	}
	actual, err := connection.price.Cost((connection.windowBytes + 31) / 32)
	if err != nil {
		return err
	}
	err = settleCost(connection.parentContext, connection.lease, actual, !connection.uncertain,
		costcontrol.Outcome{Cancelled: costcontrol.IsCancellation(connection.parentContext, connection.failure), Success: success && !connection.uncertain})
	if err == nil {
		connection.lease = nil
	}
	return err
}

func (connection *budgetedSpeechConnection) Receive() (Transcript, error) {
	value, err := connection.next.Receive()
	if err != nil {
		connection.mu.Lock()
		if !connection.closed {
			connection.uncertain = true
			connection.failure = err
		}
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
	connection.mu.Unlock()
	providerErr := connection.next.Close()
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return errors.Join(providerErr, connection.finishWindow(providerErr == nil && connection.failure == nil))
}

func settleCost(parent context.Context, lease *costcontrol.Lease, actual int64, known bool, outcome costcontrol.Outcome) error {
	settlement, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	return lease.Finish(settlement, actual, known, outcome)
}

var _ Rewriter = (*BudgetedRewriter)(nil)
var _ Speech = (*BudgetedSpeech)(nil)
var _ SpeechConnection = (*budgetedSpeechConnection)(nil)
