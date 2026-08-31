package contracts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEveryOperationAcceptsOnlyItsCurrentAndPreviousPromptVersions(t *testing.T) {
	t.Parallel()

	for _, operation := range OperationValues() {
		policy, ok := PolicyFor(operation)
		if !ok {
			t.Fatalf("missing policy for %s", operation)
		}
		for _, version := range []string{policy.Contract.Current, policy.Contract.Previous} {
			request := Request{
				RequestID:         "19be2f9e-bd92-4699-b561-e3816092114c",
				Operation:         operation,
				ContractVersion:   ContractVersionV1,
				PromptVersion:     version,
				Prompt:            "test",
				ResponseSchema:    json.RawMessage(`{"type":"object","additionalProperties":false}`),
				SemanticSignature: "sha256:test",
			}
			if err := request.Validate(); err != nil {
				t.Fatalf("%s version %s: %v", operation, version, err)
			}
		}
	}
}

func TestDecodeAndValidateAcceptsCurrentAndPreviousPromptVersions(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"meal-decision-v10-fresh-exploration", "meal-decision-v9"} {
		body := validRequestJSON(version)
		request, err := DecodeAndValidate(strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatalf("version %q should be supported: %v", version, err)
		}
		if request.Operation != OperationMealDecision {
			t.Fatalf("unexpected operation: %q", request.Operation)
		}
	}
}

func TestDecodeAndValidateRejectsUnknownPromptVersionWithUpgradeError(t *testing.T) {
	t.Parallel()

	body := validRequestJSON("meal-decision-v999")
	_, err := DecodeAndValidate(strings.NewReader(body), int64(len(body)))
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("expected upgrade error, got %v", err)
	}
}

func TestDecodeAndValidateRejectsGenericProxyFields(t *testing.T) {
	t.Parallel()

	body := strings.Replace(
		validRequestJSON("meal-decision-v10-fresh-exploration"),
		`"prompt":"choose dinner"`,
		`"prompt":"choose dinner","model":"arbitrary-model","providerURL":"https://example.com"`,
		1,
	)
	_, err := DecodeAndValidate(strings.NewReader(body), int64(len(body)))
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected contract violation, got %v", err)
	}
}

func TestDecodeAndValidateRejectsInlineManagedMedia(t *testing.T) {
	t.Parallel()

	body := strings.Replace(
		validRequestJSON("meal-decision-v10-fresh-exploration"),
		`"media":[]`,
		`"media":[{"id":"meal-photo","kind":"image","mimeType":"image/jpeg","inlineBase64":"AAAA"}]`,
		1,
	)
	_, err := DecodeAndValidate(strings.NewReader(body), int64(len(body)))
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected contract violation, got %v", err)
	}
}

func TestDecodeAndValidateRejectsBodyAboveConfiguredLimit(t *testing.T) {
	t.Parallel()

	body := validRequestJSON("meal-decision-v10-fresh-exploration")
	_, err := DecodeAndValidate(strings.NewReader(body), 32)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected payload too large, got %v", err)
	}
}

func TestRequestRejectsPromptAboveOperationPolicy(t *testing.T) {
	t.Parallel()

	policy, _ := PolicyFor(OperationMealDecision)
	request := Request{
		RequestID:         "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:         OperationMealDecision,
		ContractVersion:   ContractVersionV1,
		PromptVersion:     policy.Contract.Current,
		Prompt:            strings.Repeat("x", policy.MaxPromptBytes+1),
		ResponseSchema:    json.RawMessage(`{"type":"object","additionalProperties":false}`),
		SemanticSignature: "sha256:test",
	}
	if err := request.Validate(); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected contract violation, got %v", err)
	}
}

func TestReservationIncludesSchemaOutputAndMediaBudgets(t *testing.T) {
	t.Parallel()

	request := Request{
		Prompt: "abc", ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Media: []Media{{Kind: "image"}, {Kind: "audio"}},
	}
	if got := ReservationTokens(request); got <= len(request.Prompt)+len(request.ResponseSchema)+4_096+1024 {
		t.Fatalf("media and safety budget missing: %d", got)
	}
}

func TestRequestRejectsDuplicateMediaIdentifiers(t *testing.T) {
	t.Parallel()

	policy, _ := PolicyFor(OperationMealPhotoCapture)
	request := Request{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: OperationMealPhotoCapture,
		ContractVersion: ContractVersionV1, PromptVersion: policy.Contract.Current, Prompt: "inspect meal",
		ResponseSchema:    json.RawMessage(`{"type":"object","additionalProperties":false}`),
		SemanticSignature: "sha256:test",
	}
	request.Media = []Media{
		{ID: "photo-1", Kind: "image", MIMEType: "image/jpeg", ObjectID: "object-1", SHA256: strings.Repeat("a", 64), SizeBytes: 1024},
		{ID: "photo-1", Kind: "image", MIMEType: "image/jpeg", ObjectID: "object-2", SHA256: strings.Repeat("b", 64), SizeBytes: 1024},
	}
	if err := request.Validate(); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected duplicate media rejection, got %v", err)
	}
	request.Media[1].ID = "photo-2"
	request.Media[1].ObjectID = request.Media[0].ObjectID
	if err := request.Validate(); !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected duplicate object rejection, got %v", err)
	}
}

func validRequestJSON(promptVersion string) string {
	return `{"requestID":"19be2f9e-bd92-4699-b561-e3816092114c",` +
		`"operation":"health.meal.decision",` +
		`"contractVersion":"ai-request-v1",` +
		`"promptVersion":"` + promptVersion + `",` +
		`"prompt":"choose dinner",` +
		`"responseSchema":{"type":"object","additionalProperties":false},` +
		`"options":{},` +
		`"media":[],` +
		`"semanticSignature":"sha256:abc"}`
}
