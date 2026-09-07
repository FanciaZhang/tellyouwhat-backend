package contracts

import (
	"encoding/json"
	"fmt"
)

// OutputBudgetV1Maximum is the server-owned quota reservation for possible
// provider output. It is deliberately an accounting estimate rather than a
// provider generation limit because Responses counts reasoning and visible
// output together, and truncation makes an otherwise valid result unusable.
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

func (request Request) OutputTokenReservation() int {
	if request.OutputBudget.Valid() {
		return request.OutputBudget.MaxTokens
	}
	return OutputBudgetV1Maximum
}

// MarshalJobRequest persists server-owned budget metadata only inside the
// encrypted job payload. Request's public JSON decoder cannot set this field.
func MarshalJobRequest(request Request) ([]byte, error) {
	if request.ExecutionPolicy != nil {
		if err := request.ExecutionPolicy.Validate(request.Operation); err != nil {
			return nil, err
		}
	}
	if !request.OutputBudget.Valid() {
		return nil, fmt.Errorf("%w: missing output budget", ErrContractViolation)
	}
	return json.Marshal(struct {
		Request
		Budget OutputBudget     `json:"_serverOutputBudget"`
		Policy *ExecutionPolicy `json:"_serverExecutionPolicy,omitempty"`
	}{Request: request, Budget: request.OutputBudget, Policy: request.ExecutionPolicy})
}

func UnmarshalJobRequest(raw []byte) (Request, error) {
	var stored struct {
		Request
		Budget OutputBudget     `json:"_serverOutputBudget"`
		Policy *ExecutionPolicy `json:"_serverExecutionPolicy,omitempty"`
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
	if stored.Policy != nil {
		return stored.Request.WithExecutionPolicy(*stored.Policy)
	}
	return stored.Request, nil
}
