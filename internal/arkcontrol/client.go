// Package arkcontrol provides the server-only Ark management boundary.
package arkcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/ark"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	"github.com/volcengine/volcengine-go-sdk/volcengine/volcengineerr"
)

var (
	ErrUnavailable = errors.New("Ark management unavailable")
	ErrCredentials = errors.New("invalid Ark management credential file")
	ErrResponse    = errors.New("invalid Ark management response")
	resourceName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Client struct {
	sdk  *ark.ARK
	gate chan struct{}
	next time.Time
}

// NewFromFile never falls back to a developer profile or environment credentials.
func NewFromFile(path string) (*Client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrCredentials
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrCredentials
	}
	var secret struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	d := json.NewDecoder(io.LimitReader(f, 8193))
	d.DisallowUnknownFields()
	if d.Decode(&secret) != nil || secret.AccessKey == "" || secret.SecretKey == "" {
		return nil, ErrCredentials
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, ErrCredentials
	}
	return newClient(secret.AccessKey, secret.SecretKey, "https://ark.cn-beijing.volcengineapi.com", &http.Client{Timeout: 15 * time.Second})
}

func newClient(ak, sk, endpoint string, transport *http.Client) (*Client, error) {
	cfg := volcengine.NewConfig().WithRegion("cn-beijing").WithEndpoint(endpoint).
		WithCredentials(credentials.NewStaticCredentials(ak, sk, "")).WithHTTPClient(transport).WithMaxRetries(0)
	s, err := session.NewSession(cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &Client{sdk: ark.New(s), gate: make(chan struct{}, 1)}, nil
}

type ModelReference struct {
	FoundationModel struct {
		Name    string `json:"Name"`
		Version string `json:"ModelVersion"`
	} `json:"FoundationModel"`
}
type Endpoint struct {
	ID             string         `json:"Id"`
	Name           string         `json:"Name"`
	Status         string         `json:"Status"`
	Model          ModelReference `json:"ModelReference"`
	SupportRolling *bool          `json:"SupportRolling"`
	RollingID      string         `json:"RollingId,omitempty"`
}
type Model struct {
	Name           string `json:"Name"`
	DisplayName    string `json:"DisplayName"`
	Vendor         string `json:"VendorName"`
	Description    string `json:"DisplayDescription"`
	PrimaryVersion string `json:"PrimaryVersion"`
}
type Version struct {
	Name         string          `json:"FoundationModelName"`
	Version      string          `json:"ModelVersion"`
	ModelID      string          `json:"ModelId"`
	Status       string          `json:"Status"`
	AccessType   string          `json:"AccessType"`
	Capabilities map[string]bool `json:"CapabilityLabels"`
	Domains      []string        `json:"Domains"`
}

// Only fixed actions are exposed. Raw upstream payloads and errors are not returned.
func (c *Client) read(ctx context.Context, action string, input map[string]interface{}, target any) error {
	if c == nil || c.sdk == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		err := c.call(ctx, action, input, target)
		var failure *APIError
		readOnly := action == "GetEndpoint" || action == "GetEndpointRolling" || action == "ListFoundationModels" || action == "ListFoundationModelVersions" || action == "ListModelActivations"
		if !readOnly || attempt >= 2 || !errors.As(err, &failure) || (failure.Status != 429 && failure.Code != "FlowLimitExceeded") {
			return err
		}
		select {
		case <-ctx.Done():
			return ErrUnavailable
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}

func (c *Client) call(ctx context.Context, action string, input map[string]interface{}, target any) error {
	// Share request pacing across inventory, previews, and the background runner.
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ErrUnavailable
	}
	if wait := time.Until(c.next); wait > 0 {
		select {
		case <-ctx.Done():
			<-c.gate
			return ErrUnavailable
		case <-time.After(wait):
		}
	}
	c.next = time.Now().Add(500 * time.Millisecond)
	<-c.gate
	output := map[string]interface{}{}
	r := c.sdk.NewRequest(&request.Operation{Name: action, HTTPMethod: "POST", HTTPPath: "/"}, &input, &output)
	r.HTTPRequest.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.SetContext(ctx)
	if err := r.Send(); err != nil {
		var failure volcengineerr.RequestFailure
		if errors.As(err, &failure) && resourceName.MatchString(failure.Code()) {
			return &APIError{Code: failure.Code(), Status: failure.StatusCode()}
		}
		return ErrUnavailable
	}
	result, ok := output["Result"]
	if !ok {
		return ErrResponse
	}
	b, err := json.Marshal(result)
	if err != nil || json.Unmarshal(b, target) != nil {
		return ErrResponse
	}
	return nil
}

func (c *Client) Endpoint(ctx context.Context, id string) (Endpoint, error) {
	var out Endpoint
	if !resourceName.MatchString(id) {
		return out, ErrResponse
	}
	err := c.read(ctx, "GetEndpoint", map[string]interface{}{"Id": id}, &out)
	if err == nil && out.ID != id {
		err = ErrResponse
	}
	return out, err
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	return pages[Model](ctx, c, "ListFoundationModels", nil)
}
func (c *Client) Versions(ctx context.Context, model string) ([]Version, error) {
	if !resourceName.MatchString(model) {
		return nil, ErrResponse
	}
	return pages[Version](ctx, c, "ListFoundationModelVersions", map[string]interface{}{"FoundationModelName": model})
}
func pages[T any](ctx context.Context, c *Client, action string, input map[string]interface{}) ([]T, error) {
	if input == nil {
		input = map[string]interface{}{}
	}
	size := 100
	if n, ok := input["PageSize"].(int); ok && n > 0 && n <= 100 {
		size = n
	}
	items := make([]T, 0)
	for page := 1; page <= 100; page++ {
		input["PageNumber"], input["PageSize"] = page, size
		var out struct {
			Total int `json:"TotalCount"`
			Items []T `json:"Items"`
		}
		if err := c.read(ctx, action, input, &out); err != nil {
			return nil, err
		}
		items = append(items, out.Items...)
		if len(items) >= out.Total {
			return items, nil
		}
		if len(out.Items) == 0 {
			return nil, ErrResponse
		}
	}
	return nil, ErrResponse
}
