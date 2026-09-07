package ark

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/contracts"
	"testing"
)

func TestFrozenPolicyOverridesChangedProviderRoute(t *testing.T) {
	req := validArkRequest()
	p := contracts.ExecutionPolicy{Version: "saved", Endpoint: "ep-original", ReasoningEffort: "minimal", TimeoutSeconds: 90}
	req, err := req.WithExecutionPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{BaseURL: "https://example.test", APIKey: "test", Routes: map[contracts.Operation]Route{req.Operation: {Model: "ep-new", TimeoutSeconds: 20}}}, nil, nil)
	for _, stream := range []bool{false, true} {
		request, cancel, err := c.makeRequest(context.Background(), req, stream)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		cancel()
		if payload["model"] != "ep-original" || payload["thinking"].(map[string]any)["type"] != "disabled" {
			t.Fatal("retry ignored frozen model/parameters")
		}
	}
}
