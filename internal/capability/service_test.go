package capability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestJobCapabilityBindsJobOperationBodyMediaAndIsSingleUse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	service := NewService([]byte("01234567890123456789012345678901"), NewMemoryUseStore(), func() time.Time { return now })
	principal := attestation.Principal{AppID: "health", KeyID: "key-1", DeviceID: "device-1", TransactionID: "transaction-1"}
	issued, err := service.Issue(principal, Binding{
		RequestID:   "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:   contracts.OperationMealDecision,
		BodyDigest:  "body-digest",
		MediaDigest: "media-digest",
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{
		JobID:       issued.JobID,
		RequestID:   "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:   contracts.OperationMealDecision,
		BodyDigest:  "body-digest",
		MediaDigest: "media-digest",
	}
	validated, err := service.Validate(issued.Token, binding)
	if err != nil || validated.KeyID != principal.KeyID {
		t.Fatalf("validate: %+v, %v", validated, err)
	}
	consumed, err := service.Consume(context.Background(), issued.Token, Binding{
		JobID:       issued.JobID,
		RequestID:   "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:   contracts.OperationMealDecision,
		BodyDigest:  "body-digest",
		MediaDigest: "media-digest",
	})
	if err != nil || consumed.KeyID != principal.KeyID {
		t.Fatalf("consume: %+v, %v", consumed, err)
	}
	if _, err := service.Consume(context.Background(), issued.Token, Binding{
		JobID: issued.JobID, RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision, BodyDigest: "body-digest", MediaDigest: "media-digest",
	}); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay error, got %v", err)
	}
}

func TestJobCapabilityRejectsTamperedBodyDigest(t *testing.T) {
	t.Parallel()

	service := NewService([]byte("01234567890123456789012345678901"), NewMemoryUseStore(), time.Now)
	issued, _ := service.Issue(attestation.Principal{AppID: "health", KeyID: "key-1", DeviceID: "device-1"}, Binding{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision, BodyDigest: "body", MediaDigest: "media",
	})
	_, err := service.Consume(context.Background(), issued.Token, Binding{
		JobID: issued.JobID, RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision, BodyDigest: "tampered", MediaDigest: "media",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid capability, got %v", err)
	}
}

func TestJobCapabilityReissuesSameTokenForPersistedAdmissionTime(t *testing.T) {
	t.Parallel()

	issuedAt := time.Date(2026, 8, 2, 8, 0, 0, 123456000, time.UTC)
	now := issuedAt.Add(time.Minute)
	service := NewService([]byte("01234567890123456789012345678901"), NewMemoryUseStore(), func() time.Time { return now })
	principal := attestation.Principal{AppID: "health", KeyID: "key-1", DeviceID: "device-1", TransactionID: "transaction-1"}
	binding := Binding{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision,
		BodyDigest: "body-digest", MediaDigest: "media-digest",
	}
	first, err := service.IssueAt(principal, binding, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.IssueAt(principal, binding, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same durable admission produced different capabilities: %+v != %+v", first, second)
	}
}
