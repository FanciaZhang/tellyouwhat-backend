package capability

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestCapabilityAuthenticatesFrozenOutputBudget(t *testing.T) {
	now := time.Now().UTC()
	service := NewService([]byte("01234567890123456789012345678901"), NewMemoryUseStore(), func() time.Time { return now })
	principal := attestation.Principal{AppID: "health", KeyID: "key", DeviceID: "device"}
	binding := Binding{RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealDecision, BodyDigest: "body", MediaDigest: "media"}
	budget := contracts.OutputBudget{Version: "health-output-v1", MaxTokens: 16_384}
	issued, err := service.IssueWithOutputBudgetAt(principal, binding, now, budget)
	if err != nil {
		t.Fatal(err)
	}
	binding.JobID = issued.JobID
	_, restored, err := service.ValidateWithOutputBudget(issued.Token, binding)
	if err != nil || restored != budget || restored == contracts.DefaultOutputBudget() {
		t.Fatalf("original budget not authenticated: %+v err=%v", restored, err)
	}
	payload, signature, ok := splitToken(issued.Token)
	if !ok {
		t.Fatal("invalid issued token")
	}
	var value claims
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	value.OutputBudget.MaxTokens = contracts.OutputBudgetV1Maximum
	tamperedPayload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	tampered := base64.RawURLEncoding.EncodeToString(tamperedPayload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, _, err := service.ValidateWithOutputBudget(tampered, binding); !errors.Is(err, ErrInvalid) {
		t.Fatalf("client changed signed budget: %v", err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(payload, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "outputBudget")
	legacyPayload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyToken := base64.RawURLEncoding.EncodeToString(legacyPayload) + "." + base64.RawURLEncoding.EncodeToString(service.sign(legacyPayload))
	if _, err := service.Validate(legacyToken, binding); err != nil {
		t.Fatalf("authentic legacy token became unreadable: %v", err)
	}
	if _, _, err := service.ValidateWithOutputBudget(legacyToken, binding); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy token was given an invented output budget: %v", err)
	}
}
