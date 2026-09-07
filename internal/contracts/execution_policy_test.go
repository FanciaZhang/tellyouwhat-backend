package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExecutionPolicyJobRoundTripAndPublicBoundary(t *testing.T) {
	p := ExecutionPolicy{Version: "revision-1", Endpoint: "ep-old", ReasoningEffort: "low", TimeoutSeconds: 90}
	request, err := (Request{Operation: OperationMealTextCapture}).FreezeOutputBudget().WithExecutionPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "ep-old") || strings.Contains(string(public), "revision-1") {
		t.Fatal("server metadata escaped public request")
	}
	raw, err := MarshalJobRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := UnmarshalJobRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ExecutionPolicy == nil || *restored.ExecutionPolicy != p || restored.Options.ReasoningEffort != "low" {
		t.Fatal("lost frozen policy")
	}
}

func TestExecutionPolicyEnforcesOperationSearchBoundary(t *testing.T) {
	p := ExecutionPolicy{Version: "r1", Endpoint: "ep-test", ReasoningEffort: "minimal", TimeoutSeconds: 90, WebSearchEnabled: true}
	if p.Validate(OperationMealTextCapture) == nil {
		t.Fatal("enabled unsupported search")
	}
	if err := p.Validate(OperationMealDecision); err != nil {
		t.Fatal(err)
	}
	p.TimeoutSeconds = 0
	if p.Validate(OperationMealDecision) == nil {
		t.Fatal("accepted missing timeout")
	}
}
