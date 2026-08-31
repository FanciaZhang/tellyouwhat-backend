package appstore

import (
	"context"
	"errors"
)

type SubscriptionResolving interface {
	Resolve(context.Context, string) (SubscriptionState, error)
}

type NotificationProcessing interface {
	Process(context.Context, string) (NotificationResult, error)
}

type MultiEnvironmentSubscriptionResolver struct {
	resolvers []SubscriptionResolving
}

type MultiEnvironmentNotificationProcessor struct {
	processors []NotificationProcessing
}

func NewMultiEnvironmentSubscriptionResolver(
	resolvers ...SubscriptionResolving,
) *MultiEnvironmentSubscriptionResolver {
	return &MultiEnvironmentSubscriptionResolver{resolvers: resolvers}
}

func NewMultiEnvironmentNotificationProcessor(
	processors ...NotificationProcessing,
) *MultiEnvironmentNotificationProcessor {
	return &MultiEnvironmentNotificationProcessor{processors: processors}
}

func (resolver *MultiEnvironmentSubscriptionResolver) Resolve(
	ctx context.Context,
	signedTransaction string,
) (SubscriptionState, error) {
	if resolver == nil || len(resolver.resolvers) == 0 {
		return SubscriptionState{}, ErrStatusUnavailable
	}
	for _, candidate := range resolver.resolvers {
		if candidate == nil {
			continue
		}
		state, err := candidate.Resolve(ctx, signedTransaction)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, ErrInvalidSignedData) {
			return SubscriptionState{}, err
		}
	}
	return SubscriptionState{}, ErrInvalidSignedData
}

func (processor *MultiEnvironmentNotificationProcessor) Process(
	ctx context.Context,
	signedPayload string,
) (NotificationResult, error) {
	if processor == nil || len(processor.processors) == 0 {
		return NotificationResult{}, ErrStatusUnavailable
	}
	for _, candidate := range processor.processors {
		if candidate == nil {
			continue
		}
		result, err := candidate.Process(ctx, signedPayload)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ErrInvalidSignedData) {
			return NotificationResult{}, err
		}
	}
	return NotificationResult{}, ErrInvalidSignedData
}
