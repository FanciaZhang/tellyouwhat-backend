package contracts

import "fmt"

// ExecutionPolicy is trusted metadata stored only in encrypted job payloads.
// Endpoint is fixed for retries; Ark native rolling may still choose another model.
type ExecutionPolicy struct {
	Version          string `json:"version"`
	Endpoint         string `json:"endpoint"`
	ReasoningEffort  string `json:"reasoningEffort"`
	WebSearchEnabled bool   `json:"webSearchEnabled"`
	TimeoutSeconds   int    `json:"timeoutSeconds"`
}

func (p ExecutionPolicy) Validate(operation Operation) error {
	policy, ok := PolicyFor(operation)
	if !ok || p.Version == "" || p.Endpoint == "" || len(p.Endpoint) > 128 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 840 {
		return fmt.Errorf("%w: execution policy", ErrContractViolation)
	}
	switch p.ReasoningEffort {
	case "", "minimal", "low", "medium", "high", "max":
	default:
		return fmt.Errorf("%w: reasoning policy", ErrContractViolation)
	}
	if p.WebSearchEnabled && !policy.AllowsWebSearch {
		return fmt.Errorf("%w: search policy", ErrContractViolation)
	}
	return nil
}

func (request Request) WithExecutionPolicy(p ExecutionPolicy) (Request, error) {
	if err := p.Validate(request.Operation); err != nil {
		return Request{}, err
	}
	request.ExecutionPolicy = &p
	request.Options.ReasoningEffort = p.ReasoningEffort
	request.Options.WebSearchEnabled = p.WebSearchEnabled
	return request, nil
}
