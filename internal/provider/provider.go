package provider

import (
	"context"

	"github.com/tellyouwhat/backend/internal/contracts"
)

type Response struct {
	Content      string `json:"content"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
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
