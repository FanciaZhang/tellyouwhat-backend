package healthhttpapi

import (
	"context"
	"testing"
)

func TestEmbeddedContractIsValidAndEveryOperationIsNamed(t *testing.T) {
	t.Parallel()

	document, err := GetSwagger()
	if err != nil {
		t.Fatalf("parse embedded OpenAPI contract: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate embedded OpenAPI contract: %v", err)
	}

	const expectedOperationCount = 9
	operationIDs := make(map[string]struct{}, expectedOperationCount)
	operationCount := 0
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			operationCount++
			if operation.OperationID == "" {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			if _, exists := operationIDs[operation.OperationID]; exists {
				t.Fatalf("duplicate operationId %q", operation.OperationID)
			}
			operationIDs[operation.OperationID] = struct{}{}
		}
	}
	if operationCount != expectedOperationCount {
		t.Fatalf("operation count = %d, want %d", operationCount, expectedOperationCount)
	}
}
