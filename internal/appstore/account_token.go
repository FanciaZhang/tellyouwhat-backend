package appstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// A successful PUT is an acknowledgement only. Callers must verify a freshly
// signed current transaction before treating the new account binding as proven.
func (client *APIClient) SetAppAccountToken(ctx context.Context, originalID, accountToken string) error {
	token, err := uuid.Parse(accountToken)
	if client == nil || !client.validConfig() || originalID == "" || len(originalID) > 128 || err != nil || token == uuid.Nil || token.String() != accountToken {
		return ErrInvalidAPIResponse
	}
	bearer, err := client.bearerToken()
	if err != nil {
		return ErrStatusUnavailable
	}
	body, _ := json.Marshal(struct {
		Token string `json:"appAccountToken"`
	}{accountToken})
	endpoint := strings.TrimRight(client.config.BaseURL, "/") + "/inApps/v1/transactions/" + url.PathEscape(originalID) + "/appAccountToken"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrStatusUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	transport := *client.config.HTTPClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request)
	if err != nil {
		return ErrStatusUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrStatusUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || len(bytes.TrimSpace(data)) != 0 {
		return ErrInvalidAPIResponse
	}
	return nil
}
