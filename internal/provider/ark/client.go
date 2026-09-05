package ark

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
)

var ErrProviderConfiguration = errors.New("ark provider configuration error")

type Route struct {
	Model          string
	TimeoutSeconds int
}

type Config struct {
	BaseURL string
	APIKey  string
	Routes  map[contracts.Operation]Route
}

type MediaResolver interface {
	Resolve(context.Context, contracts.Media) (string, error)
}

type MediaCleaner interface {
	Delete(context.Context, contracts.Media) error
}

type Client struct {
	config   Config
	http     *http.Client
	resolver MediaResolver
}

func New(config Config, httpClient *http.Client, resolver MediaResolver) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{config: config, http: httpClient, resolver: resolver}
}

func (client *Client) Complete(ctx context.Context, request contracts.Request) (providerapi.Response, error) {
	httpRequest, cancel, err := client.makeRequest(ctx, request, false)
	if err != nil {
		return providerapi.Response{}, err
	}
	defer cancel()
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return providerapi.Response{}, fmt.Errorf("ark request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return providerapi.Response{}, fmt.Errorf("read ark response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerapi.Response{}, fmt.Errorf("ark status %d", response.StatusCode)
	}
	parsed, err := parseResponse(body)
	if err != nil {
		return providerapi.Response{}, err
	}
	if repaired, ok := contracts.RepairUnquotedStringValues(parsed.Content); ok {
		parsed.Content = repaired
	}
	if canonicalized, ok := contracts.CanonicalizeFoodGroupSynonyms(parsed.Content); ok {
		parsed.Content = canonicalized
	}
	if err := contracts.ValidateResponse(request, parsed.Content); err != nil {
		return providerapi.Response{}, fmt.Errorf("ark structured response: %w", err)
	}
	return parsed, nil
}

func (client *Client) Stream(
	ctx context.Context,
	request contracts.Request,
	yield func(providerapi.StreamEvent) error,
) error {
	httpRequest, cancel, err := client.makeRequest(ctx, request, true)
	if err != nil {
		return err
	}
	defer cancel()
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("ark stream request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ark stream status %d", response.StatusCode)
	}
	var dataLines []string
	dataBytes := 0
	consume := func() (*providerapi.Response, error) {
		payload := strings.Join(dataLines, "\n")
		dataLines, dataBytes = nil, 0
		if payload == "" || payload == "[DONE]" {
			return nil, nil
		}
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, fmt.Errorf("decode ark SSE: %w", err)
		}
		switch event.Type {
		case "error", "response.failed", "response.incomplete", "response.refusal.delta", "response.refusal.done":
			return nil, errors.New("ark stream did not complete successfully")
		case "response.output_text.delta":
			if event.Delta != "" {
				return nil, yield(providerapi.StreamEvent{Delta: event.Delta})
			}
		case "response.completed":
			result, err := parseResponse(event.Response)
			if err != nil {
				return nil, err
			}
			return &result, nil
		}
		return nil, nil
	}
	complete := func(result *providerapi.Response) error {
		if repaired, ok := contracts.RepairUnquotedStringValues(result.Content); ok {
			result.Content = repaired
		}
		if canonicalized, ok := contracts.CanonicalizeFoodGroupSynonyms(result.Content); ok {
			result.Content = canonicalized
		}
		if err := contracts.ValidateResponse(request, result.Content); err != nil {
			return fmt.Errorf("ark streamed structured response: %w", err)
		}
		return yield(providerapi.StreamEvent{Completed: result})
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			result, err := consume()
			if err != nil {
				return err
			}
			if result != nil {
				return complete(result)
			}
		} else if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			dataBytes += len(payload) + 1
			if dataBytes > 4<<20 {
				return errors.New("ark stream event is too large")
			}
			dataLines = append(dataLines, payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read ark SSE: %w", err)
	}
	result, err := consume()
	if err != nil {
		return err
	}
	if result != nil {
		return complete(result)
	}
	return errors.New("ark stream ended without completion")
}

func (client *Client) CleanupManagedMedia(ctx context.Context, values []contracts.Media) {
	cleaner, ok := client.resolver.(MediaCleaner)
	if !ok {
		return
	}
	for _, value := range values {
		_ = cleaner.Delete(ctx, value)
	}
}

var _ providerapi.ManagedMediaCleaner = (*Client)(nil)

func (client *Client) makeRequest(
	ctx context.Context,
	request contracts.Request,
	stream bool,
) (*http.Request, context.CancelFunc, error) {
	route, ok := client.config.Routes[request.Operation]
	if !ok || strings.TrimSpace(route.Model) == "" || strings.TrimSpace(client.config.BaseURL) == "" || client.config.APIKey == "" {
		return nil, nil, ErrProviderConfiguration
	}
	content := make([]map[string]any, 0, len(request.Media)+1)
	for _, media := range request.Media {
		if client.resolver == nil {
			return nil, nil, ErrProviderConfiguration
		}
		mediaURL, err := client.resolver.Resolve(ctx, media)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve managed media: %w", err)
		}
		switch media.Kind {
		case "image":
			content = append(content, map[string]any{"type": "input_image", "image_url": mediaURL})
		case "audio":
			content = append(content, map[string]any{"type": "input_audio", "audio_url": mediaURL})
		default:
			return nil, nil, ErrProviderConfiguration
		}
	}
	// Preserve the Swift request semantics: media is presented before the
	// canonical prompt for both BYOK and managed delivery.
	content = append(content, map[string]any{"type": "input_text", "text": request.Prompt})
	var schema any
	if err := json.Unmarshal(request.ResponseSchema, &schema); err != nil {
		return nil, nil, err
	}
	body := map[string]any{
		"model":   route.Model,
		"store":   false,
		"caching": map[string]string{"type": "disabled"},
		"input": []map[string]any{{
			"role":    "user",
			"content": content,
		}},
		"temperature": request.Options.TemperatureValue(),
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   schemaName(request.Operation),
				"strict": true,
				"schema": schema,
			},
		},
	}
	if stream {
		body["stream"] = true
	}
	if request.Options.ReasoningEffort != "" {
		if request.Options.ReasoningEffort == "minimal" {
			body["thinking"] = map[string]string{"type": "disabled"}
		} else {
			body["thinking"] = map[string]string{"type": "enabled"}
			body["reasoning"] = map[string]string{"effort": request.Options.ReasoningEffort}
		}
	}
	if request.Options.WebSearchEnabled {
		body["tools"] = []map[string]string{{"type": "web_search"}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	endpoint := strings.TrimRight(client.config.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/responses") {
		endpoint += "/api/v3/responses"
	}
	timeout := time.Duration(route.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if stream {
		httpRequest.Header.Set("Accept", "text/event-stream")
	}
	return httpRequest, cancel, nil
}

func schemaName(operation contracts.Operation) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(string(operation))
}

func parseResponse(body []byte) (providerapi.Response, error) {
	var value struct {
		Status     string          `json:"status"`
		Error      json.RawMessage `json:"error"`
		OutputText string          `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				OutputText string `json:"output_text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return providerapi.Response{}, fmt.Errorf("decode ark response: %w", err)
	}
	if value.Status != "completed" || (len(value.Error) != 0 && string(value.Error) != "null") {
		return providerapi.Response{}, errors.New("ark response did not complete successfully")
	}
	content := value.OutputText
	for _, output := range value.Output {
		for _, part := range output.Content {
			if part.Type == "refusal" {
				return providerapi.Response{}, errors.New("ark response refused the request")
			}
		}
	}
	if content == "" {
		for _, output := range value.Output {
			for _, part := range output.Content {
				if part.Text != "" {
					content += part.Text
				} else {
					content += part.OutputText
				}
			}
		}
	}
	if content == "" {
		return providerapi.Response{}, errors.New("ark response has no output text")
	}
	response := providerapi.Response{Content: content}
	if value.Usage != nil && value.Usage.InputTokens != nil && value.Usage.OutputTokens != nil {
		response.InputTokens = *value.Usage.InputTokens
		response.OutputTokens = *value.Usage.OutputTokens
		if _, known := response.KnownTokenTotal(); !known {
			response.InputTokens, response.OutputTokens = 0, 0
		}
	}
	return response, nil
}

var _ providerapi.Client = (*Client)(nil)
