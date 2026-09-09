package capability

import (
	"context"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"testing"
	"time"
)

func TestResultCapabilityIsReadOnlyBoundReusableAndExpires(t *testing.T) {
	now := time.Now()
	service := NewService([]byte("01234567890123456789012345678901"), NewMemoryUseStore(), func() time.Time { return now })
	principal := attestation.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
	binding := Binding{RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision, BodyDigest: "body", MediaDigest: "media"}
	issued, err := service.Issue(principal, binding)
	if err != nil {
		t.Fatal(err)
	}
	binding.JobID = issued.JobID
	if _, err := service.Consume(context.Background(), issued.Token, binding); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, requestID, err := service.ValidateResult(issued.ResultToken, issued.JobID)
		if err != nil || got != principal || requestID != binding.RequestID {
			t.Fatalf("result read: %v", err)
		}
	}
	if _, _, err := service.ValidateWithOutputBudget(issued.ResultToken, binding); err == nil {
		t.Fatal("result token authorized execution")
	}
	if _, _, err := service.ValidateResult(issued.Token, issued.JobID); err == nil {
		t.Fatal("upload token authorized result read")
	}
	if _, _, err := service.ValidateResult(issued.ResultToken, binding.RequestID); err == nil {
		t.Fatal("wrong job accepted")
	}
	if _, _, err := service.ValidateResult(issued.ResultToken+"x", issued.JobID); err == nil {
		t.Fatal("tampered token accepted")
	}
	now = issued.ExpiresAt
	if _, _, err := service.ValidateResult(issued.ResultToken, issued.JobID); err != ErrExpired {
		t.Fatalf("expiry: %v", err)
	}
}
