package contracts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOutputBudgetCannotBeSetThroughPublicRequestJSON(t *testing.T) {
	request, err := DecodeAndValidate(strings.NewReader(validRequestJSON("meal-decision-v10-fresh-exploration")), DefaultBodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	request = request.FreezeOutputBudget()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "outputBudget") || strings.Contains(string(raw), "maxTokens") || strings.Contains(string(raw), "_serverOutputBudget") {
		t.Fatal("public request JSON exposed internal budget metadata")
	}
	for _, field := range []string{"outputBudget", "_serverOutputBudget", "max_output_tokens"} {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		value[field] = map[string]any{"version": "health-output-v1", "maxTokens": 1}
		tampered, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeAndValidate(strings.NewReader(string(tampered)), DefaultBodyLimit); !errors.Is(err, ErrContractViolation) {
			t.Fatalf("client supplied %s was accepted: %v", field, err)
		}
	}
}

func TestEncryptedJobPayloadRoundTripPreservesAdmittedOutputBudget(t *testing.T) {
	request := Request{Prompt: "synthetic meal", ResponseSchema: json.RawMessage(`{"type":"object"}`), OutputBudget: OutputBudget{Version: "health-output-v1", MaxTokens: 16_384}}
	raw, err := MarshalJobRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalJobRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.OutputBudget != request.OutputBudget || restored.OutputTokenLimit() != 16_384 || restored.FreezeOutputBudget().OutputBudget != request.OutputBudget || ReservationTokens(restored) != ReservationTokens(request) {
		t.Fatal("stored budget was replaced by current defaults")
	}
	legacy, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	older, err := UnmarshalJobRequest(legacy)
	if err != nil || older.Prompt != request.Prompt || older.OutputBudget != (OutputBudget{}) {
		t.Fatalf("legacy data was unreadable or given an invented budget: %+v err=%v", older.OutputBudget, err)
	}
}

func TestOutputBudgetRejectsUnknownVersionAndUnsafeLimits(t *testing.T) {
	for _, budget := range []OutputBudget{
		{}, {Version: "unknown", MaxTokens: 1}, {Version: "health-output-v1", MaxTokens: -1},
		{Version: "health-output-v1", MaxTokens: 0}, {Version: "health-output-v1", MaxTokens: OutputBudgetV1Maximum + 1},
	} {
		if budget.Valid() {
			t.Fatalf("unsafe budget is valid: %+v", budget)
		}
		if _, err := MarshalJobRequest(Request{OutputBudget: budget}); !errors.Is(err, ErrContractViolation) {
			t.Fatalf("unsafe budget persisted: %+v err=%v", budget, err)
		}
	}
}
