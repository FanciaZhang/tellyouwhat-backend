package ark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tellyouwhat/backend/internal/contracts"
)

type fixedMediaResolver struct{}

func (fixedMediaResolver) Resolve(_ context.Context, value contracts.Media) (string, error) {
	return "https://media.example/" + value.ObjectID, nil
}

func TestClientUsesServerOwnedEndpointAndStrictSchema(t *testing.T) {
	t.Parallel()

	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("missing server credential")
		}
		if err := json.NewDecoder(request.Body).Decode(&upstreamBody); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"output_text":"{\"choice\":\"soup\"}","usage":{"input_tokens":12,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	client := New(Config{
		BaseURL: upstream.URL,
		APIKey:  "secret",
		Routes: map[contracts.Operation]Route{
			contracts.OperationMealDecision: {Model: "ep-managed-meal-decision", TimeoutSeconds: 5},
		},
	}, upstream.Client(), nil)
	response, err := client.Complete(context.Background(), validArkRequest())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Content != `{"choice":"soup"}` {
		t.Fatalf("unexpected content: %q", response.Content)
	}
	if upstreamBody["model"] != "ep-managed-meal-decision" {
		t.Fatalf("model was not selected by server policy: %#v", upstreamBody["model"])
	}
	text, ok := upstreamBody["text"].(map[string]any)
	if !ok {
		t.Fatalf("missing structured output: %#v", upstreamBody["text"])
	}
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("schema was not forwarded strictly: %#v", format)
	}
}

func TestCompleteRepairsUnquotedStringValuesBeforeSchemaValidation(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"output_text":"{\"choice\":soup\"}","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	client := New(Config{
		BaseURL: upstream.URL,
		APIKey:  "secret",
		Routes: map[contracts.Operation]Route{
			contracts.OperationMealDecision: {Model: "ep-managed-meal-decision", TimeoutSeconds: 5},
		},
	}, upstream.Client(), nil)
	response, err := client.Complete(context.Background(), validArkRequest())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Content != `{"choice":"soup"}` {
		t.Fatalf("unexpected repaired content: %q", response.Content)
	}
}

func TestCompleteCanonicalizesFoodGroupSynonymsBeforeSchemaValidation(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"output_text":"{\"group\":\"legumes\"}","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	client := New(Config{
		BaseURL: upstream.URL,
		APIKey:  "secret",
		Routes: map[contracts.Operation]Route{
			contracts.OperationMealDecision: {Model: "ep-managed-meal-decision", TimeoutSeconds: 5},
		},
	}, upstream.Client(), nil)
	request := validArkRequest()
	request.ResponseSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"group":{"type":"string","enum":["soy"]}},"required":["group"]}`)
	response, err := client.Complete(context.Background(), request)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if response.Content != `{"group":"soy"}` {
		t.Fatalf("unexpected canonicalized content: %q", response.Content)
	}
}

func TestClientRejectsOperationWithoutServerRoute(t *testing.T) {
	t.Parallel()

	client := New(Config{BaseURL: "https://ark.example", APIKey: "secret"}, http.DefaultClient, nil)
	_, err := client.Complete(context.Background(), validArkRequest())
	if err == nil {
		t.Fatal("operation without a fixed route must be rejected")
	}
}

func TestClientPreservesSwiftMediaBeforePromptOrdering(t *testing.T) {
	t.Parallel()

	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&upstreamBody); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"output_text":"{\"choice\":\"soup\"}"}`))
	}))
	defer upstream.Close()

	client := New(Config{
		BaseURL: upstream.URL,
		APIKey:  "secret",
		Routes: map[contracts.Operation]Route{
			contracts.OperationMealDecision: {Model: "ep-managed-meal-decision", TimeoutSeconds: 5},
		},
	}, upstream.Client(), fixedMediaResolver{})
	request := validArkRequest()
	request.Media = []contracts.Media{{
		ID: "menu", Kind: "image", MIMEType: "image/jpeg", ObjectID: "device/job/menu",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: 123,
	}}
	if _, err := client.Complete(context.Background(), request); err != nil {
		t.Fatalf("complete: %v", err)
	}
	input, _ := upstreamBody["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("unexpected input: %#v", upstreamBody["input"])
	}
	message, _ := input[0].(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("unexpected content: %#v", message["content"])
	}
	media, _ := content[0].(map[string]any)
	prompt, _ := content[1].(map[string]any)
	if media["type"] != "input_image" || prompt["type"] != "input_text" {
		t.Fatalf("managed multimodal order drifted: %#v", content)
	}
}

func validArkRequest() contracts.Request {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"choice":{"type":"string"}},"required":["choice"]}`)
	return contracts.Request{
		RequestID:         "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation:         contracts.OperationMealDecision,
		ContractVersion:   contracts.ContractVersionV1,
		PromptVersion:     "meal-decision-v10-fresh-exploration",
		Prompt:            "choose dinner",
		ResponseSchema:    schema,
		SemanticSignature: "sha256:abc",
	}
}
