package provider

import (
	"context"
	"math"

	"github.com/tellyouwhat/backend/internal/contracts"
)

type Response struct {
	Content      string `json:"content"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
}

// KnownTokenTotal excludes absent, invalid or overflowing provider usage.
// Callers retain the original reservation when metering is unavailable.
func (response Response) KnownTokenTotal() (int, bool) {
	if response.InputTokens < 0 || response.OutputTokens < 0 || response.InputTokens > math.MaxInt-response.OutputTokens {
		return 0, false
	}
	total := response.InputTokens + response.OutputTokens
	return total, total > 0
}

type StreamEvent struct {
	Delta     string
	Completed *Response
}

type Client interface {
	Complete(context.Context, contracts.Request) (Response, error)
	Stream(context.Context, contracts.Request, func(StreamEvent) error) error
}

type ManagedMediaCleaner interface {
	CleanupManagedMedia(context.Context, []contracts.Media)
}
