package appstore

import (
	"context"
	"errors"
	"testing"
)

type subscriptionResolverStub func(context.Context, string) (SubscriptionState, error)

func (stub subscriptionResolverStub) Resolve(ctx context.Context, signed string) (SubscriptionState, error) {
	return stub(ctx, signed)
}

type notificationProcessorStub func(context.Context, string) (NotificationResult, error)

func (stub notificationProcessorStub) Process(ctx context.Context, signed string) (NotificationResult, error) {
	return stub(ctx, signed)
}

func TestMultiEnvironmentSubscriptionResolverFallsBackOnlyForEnvironmentMismatch(t *testing.T) {
	t.Parallel()

	sandboxCalled := false
	resolver := NewMultiEnvironmentSubscriptionResolver(
		subscriptionResolverStub(func(context.Context, string) (SubscriptionState, error) {
			return SubscriptionState{}, ErrInvalidSignedData
		}),
		subscriptionResolverStub(func(context.Context, string) (SubscriptionState, error) {
			sandboxCalled = true
			return SubscriptionState{Environment: "Sandbox"}, nil
		}),
	)
	state, err := resolver.Resolve(context.Background(), "sandbox-jws")
	if err != nil || state.Environment != "Sandbox" || !sandboxCalled {
		t.Fatalf("sandbox fallback failed: state=%+v err=%v called=%t", state, err, sandboxCalled)
	}

	sandboxCalled = false
	resolver = NewMultiEnvironmentSubscriptionResolver(
		subscriptionResolverStub(func(context.Context, string) (SubscriptionState, error) {
			return SubscriptionState{}, ErrStatusUnavailable
		}),
		subscriptionResolverStub(func(context.Context, string) (SubscriptionState, error) {
			sandboxCalled = true
			return SubscriptionState{}, nil
		}),
	)
	_, err = resolver.Resolve(context.Background(), "production-jws")
	if !errors.Is(err, ErrStatusUnavailable) || sandboxCalled {
		t.Fatalf("operational production failure incorrectly fell back: %v called=%t", err, sandboxCalled)
	}
}

func TestMultiEnvironmentNotificationProcessorFallsBackForSandboxEnvelope(t *testing.T) {
	t.Parallel()

	processor := NewMultiEnvironmentNotificationProcessor(
		notificationProcessorStub(func(context.Context, string) (NotificationResult, error) {
			return NotificationResult{}, ErrInvalidSignedData
		}),
		notificationProcessorStub(func(context.Context, string) (NotificationResult, error) {
			return NotificationResult{Environment: "Sandbox", NotificationUUID: "notification"}, nil
		}),
	)
	result, err := processor.Process(context.Background(), "sandbox-notification-jws")
	if err != nil || result.Environment != "Sandbox" {
		t.Fatalf("sandbox notification fallback failed: result=%+v err=%v", result, err)
	}
}
