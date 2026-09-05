package ark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
)

func TestEveryHealthRouteSendsOutputLimitCoveredByReservation(t *testing.T) {
	for _, operation := range contracts.OperationValues() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", operation, stream), func(t *testing.T) {
				bodies := make(chan map[string]any, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					bodies <- body
					response := `{"status":"completed","output_text":"{\"choice\":\"soup\"}","usage":{"input_tokens":12,"output_tokens":5}}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					} else {
						_, _ = fmt.Fprint(w, response)
					}
				}))
				defer upstream.Close()
				client := New(Config{BaseURL: upstream.URL, APIKey: "fixture", Routes: map[contracts.Operation]Route{
					operation: {Model: "synthetic-model", TimeoutSeconds: 5},
				}}, upstream.Client(), nil)
				request := validArkRequest()
				request.Operation = operation
				var err error
				if stream {
					err = client.Stream(context.Background(), request, func(providerapi.StreamEvent) error { return nil })
				} else {
					_, err = client.Complete(context.Background(), request)
				}
				if err != nil {
					t.Fatal(err)
				}
				body := <-bodies
				limit, ok := body["max_output_tokens"].(float64)
				if !ok || limit != 65_536 {
					t.Fatalf("provider output is not bounded: %v", body["max_output_tokens"])
				}
				minimum := len(request.Prompt) + len(request.ResponseSchema) + int(limit) + 1024
				if reserved := contracts.ReservationTokens(request); reserved < minimum {
					t.Fatalf("reservation %d does not cover provider output limit %d", reserved, int(limit))
				}
			})
		}
	}
}

func TestProviderUsesPersistedOutputBudgetInsteadOfCurrentDefault(t *testing.T) {
	request := validArkRequest()
	request.OutputBudget = contracts.OutputBudget{Version: "health-output-v1", MaxTokens: 16_384}
	raw, err := contracts.MarshalJobRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := contracts.UnmarshalJobRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{BaseURL: "https://ark.test", APIKey: "fixture", Routes: map[contracts.Operation]Route{
		request.Operation: {Model: "fixture-model", TimeoutSeconds: 5},
	}}, nil, nil)
	httpRequest, cancel, err := client.makeRequest(context.Background(), restored, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer httpRequest.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(httpRequest.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["max_output_tokens"] != float64(16_384) {
		t.Fatalf("provider replaced persisted budget: %v", body["max_output_tokens"])
	}
}
