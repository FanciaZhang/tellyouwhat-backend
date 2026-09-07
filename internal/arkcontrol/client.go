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
)

var (
	ErrUnavailable = errors.New("Ark management unavailable")
	ErrCredentials = errors.New("invalid Ark management credential file")
	ErrResponse    = errors.New("invalid Ark management response")
	resourceName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Client struct{ sdk *ark.ARK }

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
	return &Client{sdk: ark.New(s)}, nil
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
	Name        string `json:"Name"`
	DisplayName string `json:"DisplayName"`
}
type Version struct {
	Name         string          `json:"FoundationModelName"`
	Version      string          `json:"ModelVersion"`
	ModelID      string          `json:"ModelId"`
	Status       string          `json:"Status"`
	AccessType   string          `json:"AccessType"`
	Capabilities map[string]bool `json:"CapabilityLabels"`
}

// Only fixed read actions are exposed. Raw upstream payloads and errors are not returned.
func (c *Client) read(ctx context.Context, action string, input map[string]interface{}, target any) error {
	if c == nil || c.sdk == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output := map[string]interface{}{}
	r := c.sdk.NewRequest(&request.Operation{Name: action, HTTPMethod: "POST", HTTPPath: "/"}, &input, &output)
	r.HTTPRequest.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.SetContext(ctx)
	if r.Send() != nil {
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
	items := make([]T, 0)
	for page := 1; page <= 100; page++ {
		input["PageNumber"], input["PageSize"] = page, 100
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
