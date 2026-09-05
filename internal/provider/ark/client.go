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
	var content strings.Builder
	var usage providerapi.Response
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Text     string `json:"text"`
			Response struct {
				OutputText string `json:"output_text"`
				Usage      struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("decode ark SSE: %w", err)
		}
		if event.Delta != "" && strings.Contains(event.Type, "output_text.delta") {
			content.WriteString(event.Delta)
			if err := yield(providerapi.StreamEvent{Delta: event.Delta}); err != nil {
				return err
			}
		}
		if event.Type == "response.completed" {
			usage.InputTokens = event.Response.Usage.InputTokens
			usage.OutputTokens = event.Response.Usage.OutputTokens
			if content.Len() == 0 && event.Response.OutputText != "" {
				content.WriteString(event.Response.OutputText)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read ark SSE: %w", err)
	}
	usage.Content = content.String()
	if repaired, ok := contracts.RepairUnquotedStringValues(usage.Content); ok {
		usage.Content = repaired
	}
	if canonicalized, ok := contracts.CanonicalizeFoodGroupSynonyms(usage.Content); ok {
		usage.Content = canonicalized
	}
	if err := contracts.ValidateResponse(request, usage.Content); err != nil {
		return fmt.Errorf("ark streamed structured response: %w", err)
	}
	return yield(providerapi.StreamEvent{Completed: &usage})
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
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text       string `json:"text"`
				OutputText string `json:"output_text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return providerapi.Response{}, fmt.Errorf("decode ark response: %w", err)
	}
	content := value.OutputText
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
	return providerapi.Response{
		Content:      content,
		InputTokens:  value.Usage.InputTokens,
		OutputTokens: value.Usage.OutputTokens,
	}, nil
}

var _ providerapi.Client = (*Client)(nil)
