package ark

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/tellyouwhat/backend/internal/contracts"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestProbeUsesBoundedSyntheticProductionProtocol(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "chosen-version" || body["max_output_tokens"] != float64(2048) || body["store"] != false {
			t.Errorf("invalid probe body")
		}
		if body["reasoning"].(map[string]any)["effort"] != "high" {
			t.Error("effort lost")
		}
		response := `{"status":"completed","model":"chosen-version","output_text":"{\"ok\":true}"}`
		if body["stream"] == true {
			fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
		} else {
			fmt.Fprint(w, response)
		}
	}))
	defer server.Close()
	client := New(Config{BaseURL: server.URL, APIKey: "fixture"}, server.Client(), nil)
	p := contracts.ExecutionPolicy{Version: "test", Endpoint: "ep-old", ReasoningEffort: "high", TimeoutSeconds: 90}
	if err := client.Probe(context.Background(), "chosen-version", contracts.OperationMealPhotoCapture, p); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("both completion modes must be checked", calls)
	}
}
func TestLiveSelectedModelProtocol(t *testing.T) {
	model := os.Getenv("ARK_PROTOCOL_TEST_MODEL")
	key := os.Getenv("HEALTH_ARK_API_KEY")
	if model == "" || key == "" {
		t.Skip("explicit selected-model inference test required")
	}
	p := contracts.ExecutionPolicy{Version: "protocol-test", Endpoint: model, ReasoningEffort: os.Getenv("ARK_PROTOCOL_TEST_EFFORT"), WebSearchEnabled: os.Getenv("ARK_PROTOCOL_TEST_SEARCH") == "1", TimeoutSeconds: 25}
	op := contracts.OperationMealTextCapture
	if value := os.Getenv("ARK_PROTOCOL_TEST_OPERATION"); value != "" {
		op = contracts.Operation(value)
	}
	if err := New(Config{BaseURL: "https://ark.cn-beijing.volces.com", APIKey: key}, nil, nil).Probe(context.Background(), model, op, p); err != nil {
		t.Fatal(err)
	}
}
