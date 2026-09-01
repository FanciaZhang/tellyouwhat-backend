package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
)

var ErrRefusal = errors.New("model refused the request")
var ErrInvalidResult = errors.New("model returned an invalid structured result")

type Result struct {
	Value        contracts.ModelResult
	InputTokens  int
	OutputTokens int
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
	model := c.config.LiteModel
	if pro {
		model = c.config.ProModel
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
		return Result{}, fmt.Errorf("encode model input: %w", err)
	}
	payload := map[string]any{
		"model": model, "store": false,
		"instructions": "你为私人手记生成简洁、原文明示的标签，并推荐已有手记册或建议新手记册。不得推断疾病、诊断、政治立场或其他未明示敏感属性。不得返回 rejectedTagNames 中的标签。已有手记册只能返回输入中的别名。只输出符合 schema 的 JSON。",
		"input":        string(input),
		"text":         map[string]any{"format": map[string]any{"type": "json_schema", "name": "journal_organize", "strict": true, "schema": contracts.ResponseSchema()}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("encode Responses API request: %w", err)
	}
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
	if envelope.Status != "" && envelope.Status != "completed" {
		return Result{}, fmt.Errorf("%w: provider response status %q", ErrInvalidResult, envelope.Status)
	}
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 {
		return Result{}, fmt.Errorf("%w: provider returned negative token usage", ErrInvalidResult)
	}
	var text string
	for _, output := range envelope.Output {
		for _, content := range output.Content {
			if content.Type == "refusal" {
				return Result{}, ErrRefusal
			}
			if content.Type == "output_text" {
				text += content.Text
			}
		}
	}
	if text == "" {
		return Result{}, errors.New("provider returned no output_text")
	}
	var result contracts.ModelResult
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("%w: trailing structured output", ErrInvalidResult)
	}
	for i := range result.ExistingBookRecommendations {
		id, ok := aliases[result.ExistingBookRecommendations[i].BookID]
		if !ok {
			return Result{}, fmt.Errorf("%w: unknown book alias", ErrInvalidResult)
		}
		result.ExistingBookRecommendations[i].BookID = id
	}
	bookIDs := map[string]bool{}
	for _, book := range request.Books {
		bookIDs[book.ID] = true
	}
	if err := result.Validate(bookIDs); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidResult, err)
	}
	return Result{Value: result, InputTokens: envelope.Usage.InputTokens, OutputTokens: envelope.Usage.OutputTokens}, nil
}
