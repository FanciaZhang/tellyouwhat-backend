package arkcontrol

import (
	"context"
	"encoding/json"
	"errors"
)

// APIError contains machine-readable diagnostics only, never the vendor message.
type APIError struct {
	Code   string
	Status int
}

func (e *APIError) Error() string { return "Ark management request failed: " + e.Code }
func (e *APIError) Unwrap() error { return ErrUnavailable }

type FoundationModel struct {
	Name    string `json:"Name"`
	Version string `json:"ModelVersion"`
}
type Rolling struct {
	ID         string          `json:"Id"`
	EndpointID string          `json:"EndpointId"`
	Status     string          `json:"Status"`
	In         FoundationModel `json:"RollingIn"`
	Out        FoundationModel `json:"RollingOut"`
	Gray       int             `json:"RollingGray"`
	Strategy   struct {
		Step         int `json:"Step"`
		WaitDuration int `json:"WaitDuration"`
	} `json:"RollingStrategy"`
	CreateTime string `json:"CreateTime"`
	UpdateTime string `json:"UpdateTime"`
}

func (c *Client) Rolling(ctx context.Context, id string) (Rolling, error) {
	var out struct {
		Info Rolling `json:"RollingInfo"`
	}
	if !resourceName.MatchString(id) {
		return out.Info, ErrResponse
	}
	err := c.read(ctx, "GetEndpointRolling", map[string]interface{}{"Id": id}, &out)
	if err == nil && (out.Info.ID != id || !resourceName.MatchString(out.Info.EndpointID) || out.Info.Gray < 0 || out.Info.Gray > 100) {
		err = ErrResponse
	}
	return out.Info, err
}

// PreviewRolling performs only the documented dry run. DryRunOperation is the
// vendor's explicit success signal; an ordinary success is not assumed valid.
func (c *Client) PreviewRolling(ctx context.Context, endpoint string, target FoundationModel) error {
	_, err := c.createRolling(ctx, endpoint, target, "", true)
	var failure *APIError
	if errors.As(err, &failure) && failure.Code == "DryRunOperation" {
		return nil
	}
	if err == nil {
		return ErrResponse
	}
	return err
}

// CreateRolling must be preceded by a durable command record. The API has no
// documented client idempotency token; callers must reconcile ambiguous replies.
func (c *Client) CreateRolling(ctx context.Context, endpoint string, target FoundationModel, commandID string) (string, error) {
	if !resourceName.MatchString(commandID) {
		return "", ErrResponse
	}
	return c.createRolling(ctx, endpoint, target, "health-ai-command:"+commandID, false)
}
func (c *Client) createRolling(ctx context.Context, endpoint string, target FoundationModel, description string, dryRun bool) (string, error) {
	if !resourceName.MatchString(endpoint) || !resourceName.MatchString(target.Name) || !resourceName.MatchString(target.Version) {
		return "", ErrResponse
	}
	var result struct {
		ID string `json:"Id"`
	}
	err := c.read(ctx, "CreateEndpointRolling", map[string]interface{}{"EndpointId": endpoint, "ModelReference": map[string]interface{}{"FoundationModel": target}, "Description": description, "DryRun": dryRun}, &result)
	if err == nil && !resourceName.MatchString(result.ID) {
		err = ErrResponse
	}
	return result.ID, err
}

// CancelRolling resets the native gray ratio to zero.
func (c *Client) CancelRolling(ctx context.Context, id string) error {
	return c.rollingMutation(ctx, "CancelEndpointRolling", id)
}

// RollbackRolling goes back one native gray step. It is not a full cancellation.
func (c *Client) RollbackRolling(ctx context.Context, id string) error {
	return c.rollingMutation(ctx, "RollbackEndpointRolling", id)
}
func (c *Client) rollingMutation(ctx context.Context, action, id string) error {
	if !resourceName.MatchString(id) {
		return ErrResponse
	}
	var result struct {
		ID string `json:"Id"`
	}
	err := c.read(ctx, action, map[string]interface{}{"Id": id}, &result)
	if err == nil && result.ID != id {
		return ErrResponse
	}
	return err
}

type ChargeItem struct {
	Price         json.Number `json:"Price"`
	OriginalPrice json.Number `json:"OriginalPrice"`
	Unit          string      `json:"UnitCode"`
	Type          string      `json:"Type"`
}
type Activation struct {
	Name        string       `json:"FoundationModelName"`
	DisplayName string       `json:"DisplayName"`
	State       string       `json:"State"`
	Deprecated  bool         `json:"IsDeprecated"`
	Overdue     bool         `json:"IsOverdue"`
	Charges     []ChargeItem `json:"ChargeItems"`
	Tiers       []struct {
		Name        string       `json:"Name"`
		Description string       `json:"Description"`
		Charges     []ChargeItem `json:"ChargeItems"`
	} `json:"MultiChargeItems"`
	SubServices []struct {
		Service string `json:"SubService"`
		Status  string `json:"Status"`
	} `json:"SubServices"`
}

func (c *Client) Activations(ctx context.Context) ([]Activation, error) {
	return pages[Activation](ctx, c, "ListModelActivations", map[string]interface{}{"WithPrice": true, "WithFreeUsage": false, "PageSize": 20})
}

// ModelActivations narrows billing reads to the models used by one endpoint.
func (c *Client) ModelActivations(ctx context.Context, names []string) ([]Activation, error) {
	if len(names) == 0 || len(names) > 4 {
		return nil, ErrResponse
	}
	for _, name := range names {
		if !resourceName.MatchString(name) {
			return nil, ErrResponse
		}
	}
	return pages[Activation](ctx, c, "ListModelActivations", map[string]interface{}{"WithPrice": true, "WithFreeUsage": false, "PageSize": 20, "Filter": map[string]interface{}{"FoundationModelNames": names}})
}
