package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

var ErrRefusal = errors.New("model refused the request")
var ErrInvalidResult = errors.New("model returned an invalid structured result")

type Result struct {
	Value         contracts.ModelResult
	InputTokens   int
	OutputTokens  int
	Model         string `json:"model"`
	ConfigVersion string `json:"configVersion"`
}

type Config struct{ BaseURL, APIKey, LiteModel, ProModel string }
type Client struct {
	config Config
	http   *http.Client
}

func New(config Config, client *http.Client) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{config: config, http: client}
}

type aliasedBook struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContainsEntry bool   `json:"containsEntry"`
}
type modelInput struct {
	Title            string        `json:"title"`
	Body             string        `json:"body"`
	ExistingTags     []string      `json:"existingTags"`
	RejectedTagNames []string      `json:"rejectedTagNames"`
	Books            []aliasedBook `json:"books"`
}

func (c *Client) Organize(ctx context.Context, request contracts.OrganizeRequest, pro bool) (Result, error) {
	prepared, aliases, err := PrepareOrganize(ctx, request, pro, c.config)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(prepared.Parameters.TimeoutSeconds)*time.Second)
	defer cancel()
	encoded := prepared.Body
	url := strings.TrimRight(c.config.BaseURL, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, contracts.MaxBodyBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(body) > contracts.MaxBodyBytes {
		return Result{}, errors.New("provider response too large")
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("provider status %d", resp.StatusCode)
	}
	var envelope struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string                        `json:"type"`
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Result{}, err
	}
	metered := Result{Model: envelope.Model, ConfigVersion: prepared.Version, InputTokens: envelope.Usage.InputTokens, OutputTokens: envelope.Usage.OutputTokens}
	if envelope.Status != "" && envelope.Status != "completed" {
		return metered, fmt.Errorf("%w: provider response status %q", ErrInvalidResult, envelope.Status)
	}
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 {
		return metered, fmt.Errorf("%w: provider returned negative token usage", ErrInvalidResult)
	}
	var text string
	for _, output := range envelope.Output {
		for _, content := range output.Content {
			if content.Type == "refusal" {
				return metered, ErrRefusal
			}
			if content.Type == "output_text" {
				text += content.Text
			}
		}
	}
	if text == "" {
		return metered, errors.New("provider returned no output_text")
	}
	var result contracts.ModelResult
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return metered, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return metered, fmt.Errorf("%w: trailing structured output", ErrInvalidResult)
	}
	for i := range result.ExistingBookRecommendations {
		id, ok := aliases[result.ExistingBookRecommendations[i].BookID]
		if !ok {
			return metered, fmt.Errorf("%w: unknown book alias", ErrInvalidResult)
		}
		result.ExistingBookRecommendations[i].BookID = id
	}
	bookIDs := map[string]bool{}
	for _, book := range request.Books {
		bookIDs[book.ID] = true
	}
	if err := result.Validate(bookIDs); err != nil {
		return metered, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	for _, tag := range result.Tags {
		for _, rejected := range request.RejectedTagNames {
			if strings.EqualFold(strings.TrimSpace(tag.Name), strings.TrimSpace(rejected)) {
				return metered, fmt.Errorf("%w: rejected tag", ErrInvalidResult)
			}
		}
	}
	metered.Value = result
	return metered, nil
}

type PreparedOrganize struct {
	Body       json.RawMessage         `json:"body"`
	Parameters promptconfig.Parameters `json:"parameters"`
	Version    string                  `json:"version"`
}

func PrepareOrganize(ctx context.Context, request contracts.OrganizeRequest, pro bool, defaults Config) (PreparedOrganize, map[string]string, error) {
	settings := promptconfig.Defaults(defaults.LiteModel, defaults.ProModel, defaults.ProModel, 60)["journal"].Journal.Organize
	version := "seed"
	if r, ok := promptconfig.FromContext(ctx); ok {
		settings = r.Policy.Journal.Organize
		version = r.ID
	}
	parameters := settings.Lite
	if pro {
		parameters = settings.Pro
	}
	aliases := map[string]string{}
	books := make([]aliasedBook, 0, len(request.Books))
	for i, b := range request.Books {
		alias := fmt.Sprintf("b%d", i+1)
		aliases[alias] = b.ID
		books = append(books, aliasedBook{alias, b.Name, b.Description, b.ContainsEntry})
	}
	input, err := json.Marshal(modelInput{
		Title: request.Title, Body: request.Body,
		ExistingTags: request.ExistingTags, RejectedTagNames: request.RejectedTagNames,
		Books: books,
	})
	if err != nil {
		return PreparedOrganize{}, nil, fmt.Errorf("encode model input: %w", err)
	}
	payload := map[string]any{
		"store":        false,
		"thinking":     map[string]any{"type": "disabled"},
		"instructions": settings.Prompt,
		"input":        string(input),
		"text":         map[string]any{"format": map[string]any{"type": "json_schema", "name": "journal_organize", "strict": true, "schema": contracts.ResponseSchema()}},
	}
	parameters.Apply(payload)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return PreparedOrganize{}, nil, fmt.Errorf("encode Responses API request: %w", err)
	}

	return PreparedOrganize{Body: encoded, Parameters: parameters, Version: version}, aliases, nil
}
