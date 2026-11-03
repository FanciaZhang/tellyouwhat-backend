// Package illustration implements the Journal-only image provider boundary.
// It deliberately does not reuse text-token billing or enable any gateway route.
package illustration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxImageBytes = 25_000_000
const maxResponseBytes = 34_000_000

var ErrInput = errors.New("invalid illustration input")
var ErrResult = errors.New("invalid illustration result")

// ErrOutcomeUnknown must not trigger automatic resubmission: generation may
// already have incurred a charge even when its response could not be received.
var ErrOutcomeUnknown = errors.New("illustration outcome unknown")

type Rejected struct{ Status int }

func (e Rejected) Error() string { return "illustration provider rejected request" }

type Input struct {
	Prompt string
	// Reference is a user-confirmed image, not a URL that the service may fetch.
	Reference []byte
}
type ImageGenerator interface {
	Model() string
	Generate(context.Context, Input) (Result, error)
}
type Result struct {
	Image         []byte
	MIME          string
	Width, Height int
}
type Provider struct {
	endpoint, key, model string
	http                 *http.Client
}

func (p *Provider) Model() string { return p.model }

func NewProvider(endpoint, key, model string, transport http.RoundTripper) (*Provider, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(key) == "" || strings.TrimSpace(model) == "" {
		return nil, ErrInput
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Provider{endpoint: strings.TrimRight(endpoint, "/") + "/images/generations", key: key, model: model,
		http: &http.Client{Transport: transport, Timeout: 3 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func inspectImage(data []byte) (Result, error) {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return Result{}, ErrResult
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 16000 || config.Height > 16000 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return Result{}, ErrResult
	}
	// Header inspection bounds input here; the App performs bounded ImageIO
	// decoding before accepting a result into its encrypted document.
	return Result{Image: data, MIME: "image/" + format, Width: config.Width, Height: config.Height}, nil
}

func (p *Provider) Generate(ctx context.Context, input Input) (Result, error) {
	if strings.TrimSpace(input.Prompt) == "" || len([]rune(input.Prompt)) > 20_000 {
		return Result{}, ErrInput
	}
	payload := map[string]any{"model": p.model, "prompt": input.Prompt, "size": "2K",
		"response_format": "b64_json", "watermark": true}
	if len(input.Reference) > 0 {
		ref, err := inspectImage(input.Reference)
		if err != nil {
			return Result{}, ErrInput
		}
		payload["image"] = "data:" + ref.MIME + ";base64," + base64.StdEncoding.EncodeToString(ref.Image)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Result{}, ErrInput
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return Result{}, ErrInput
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	// One attempt only. No provider error body or prompt is included in errors.
	response, err := p.http.Do(req)
	if err != nil {
		return Result{}, ErrOutcomeUnknown
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 500 {
			return Result{}, ErrOutcomeUnknown
		}
		return Result{}, Rejected{Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return Result{}, ErrOutcomeUnknown
	}
	var body struct {
		Data []struct {
			Base64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &body) != nil || len(body.Data) != 1 || len(body.Data[0].Base64) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return Result{}, ErrResult
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(body.Data[0].Base64)
	if err != nil {
		return Result{}, ErrResult
	}
	return inspectImage(decoded)
}
