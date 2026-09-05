package contracts

import (
	"encoding/json"
	"fmt"
)

// OutputBudgetV1Maximum includes visible output and provider reasoning tokens.
// The initial ceiling preserves room for nested structured results. Lower
// operation-specific ceilings require business-result evaluation.
const OutputBudgetV1Maximum = 65_536

type OutputBudget struct {
	Version   string `json:"version"`
	MaxTokens int    `json:"maxTokens"`
}

func DefaultOutputBudget() OutputBudget {
	return OutputBudget{Version: "health-output-v1", MaxTokens: OutputBudgetV1Maximum}
}

func (budget OutputBudget) Valid() bool {
	return budget.Version == "health-output-v1" && budget.MaxTokens > 0 && budget.MaxTokens <= OutputBudgetV1Maximum
}

func (request Request) FreezeOutputBudget() Request {
	if request.OutputBudget == (OutputBudget{}) {
		request.OutputBudget = DefaultOutputBudget()
	}
	return request
}

func (request Request) OutputTokenLimit() int {
	if request.OutputBudget.Valid() {
		return request.OutputBudget.MaxTokens
	}
	return OutputBudgetV1Maximum
}

// MarshalJobRequest persists server-owned budget metadata only inside the
// encrypted job payload. Request's public JSON decoder cannot set this field.
func MarshalJobRequest(request Request) ([]byte, error) {
	if !request.OutputBudget.Valid() {
		return nil, fmt.Errorf("%w: missing output budget", ErrContractViolation)
	}
	return json.Marshal(struct {
		Request
		Budget OutputBudget `json:"_serverOutputBudget"`
	}{Request: request, Budget: request.OutputBudget})
}

func UnmarshalJobRequest(raw []byte) (Request, error) {
	var stored struct {
		Request
		Budget OutputBudget `json:"_serverOutputBudget"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Request{}, err
	}
	if stored.Budget != (OutputBudget{}) && !stored.Budget.Valid() {
		return Request{}, fmt.Errorf("%w: unsupported stored output budget", ErrContractViolation)
	}
	// A legacy job stays readable. A worker must reject new execution without
	// its original budget instead of silently choosing the active default.
	stored.Request.OutputBudget = stored.Budget
	return stored.Request, nil
}
