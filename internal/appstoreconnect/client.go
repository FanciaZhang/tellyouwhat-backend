package appstoreconnect

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultBaseURL         = "https://api.appstoreconnect.apple.com"
	tokenLifetime          = 5 * time.Minute
	maximumResponseBytes   = 2 << 20
	maximumCSVBytes        = 8 << 20
	maximumPaginationPages = 20
)

var (
	ErrUnavailable = errors.New("app store connect unavailable")
	ErrForbidden   = errors.New("app store connect operation forbidden")
	ErrInvalid     = errors.New("invalid app store connect response")
)

type Config struct {
	BaseURL        string
	IssuerID       string
	KeyID          string
	SubscriptionID string
	SigningKey     *ecdsa.PrivateKey
	HTTPClient     *http.Client
	Now            func() time.Time
}

type Client struct{ config Config }

type Offer struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	CustomerEligibilities  []string `json:"customerEligibilities"`
	OfferEligibility       string   `json:"offerEligibility"`
	Duration               string   `json:"duration"`
	OfferMode              string   `json:"offerMode"`
	NumberOfPeriods        int      `json:"numberOfPeriods"`
	TotalNumberOfCodes     int      `json:"totalNumberOfCodes"`
	ProductionCodeCount    int      `json:"productionCodeCount"`
	SandboxCodeCount       int      `json:"sandboxCodeCount"`
	Active                 bool     `json:"active"`
	AutoRenewEnabled       bool     `json:"autoRenewEnabled"`
	TargetSubscriptionPlan string   `json:"targetSubscriptionPlanType"`
}

type OfferDraft struct {
	Name                  string   `json:"name"`
	CustomerEligibilities []string `json:"customerEligibilities"`
	Duration              string   `json:"duration"`
	AutoRenewEnabled      bool     `json:"autoRenewEnabled"`
}

type CodePool struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Code           string `json:"code,omitempty"`
	NumberOfCodes  int    `json:"numberOfCodes"`
	ExpirationDate string `json:"expirationDate,omitempty"`
	Environment    string `json:"environment,omitempty"`
	Active         bool   `json:"active"`
}

type jsonAPIResource[T any] struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes T      `json:"attributes"`
}

type offerAttributes struct {
	Name                       string   `json:"name"`
	CustomerEligibilities      []string `json:"customerEligibilities"`
	OfferEligibility           string   `json:"offerEligibility"`
	Duration                   string   `json:"duration"`
	OfferMode                  string   `json:"offerMode"`
	NumberOfPeriods            int      `json:"numberOfPeriods"`
	TotalNumberOfCodes         int      `json:"totalNumberOfCodes"`
	ProductionCodeCount        int      `json:"productionCodeCount"`
	SandboxCodeCount           int      `json:"sandboxCodeCount"`
	Active                     bool     `json:"active"`
	AutoRenewEnabled           bool     `json:"autoRenewEnabled"`
	TargetSubscriptionPlanType string   `json:"targetSubscriptionPlanType"`
}

type listResponse[T any] struct {
	Data  []jsonAPIResource[T] `json:"data"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}

func NewClient(config Config) (*Client, error) {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.Host == "" ||
		config.IssuerID == "" || config.KeyID == "" || config.SubscriptionID == "" || config.SigningKey == nil {
		return nil, errors.New("invalid app store connect configuration")
	}
	return &Client{config: config}, nil
}

func (client *Client) ListOffers(ctx context.Context) ([]Offer, error) {
	path := "/v1/subscriptions/" + url.PathEscape(client.config.SubscriptionID) + "/offerCodes"
	next := strings.TrimRight(client.config.BaseURL, "/") + path + "?limit=200"
	var offers []Offer
	for page := 0; page < maximumPaginationPages && next != ""; page++ {
		var payload listResponse[offerAttributes]
		if err := client.get(ctx, next, "GET "+path, &payload); err != nil {
			return nil, err
		}
		for _, resource := range payload.Data {
			if resource.ID == "" || resource.Type != "subscriptionOfferCodes" || resource.Attributes.Name == "" {
				return nil, ErrInvalid
			}
			attributes := resource.Attributes
			offers = append(offers, Offer{
				ID: resource.ID, Name: attributes.Name,
				CustomerEligibilities: attributes.CustomerEligibilities,
				OfferEligibility:      attributes.OfferEligibility, Duration: attributes.Duration,
				OfferMode: attributes.OfferMode, NumberOfPeriods: attributes.NumberOfPeriods,
				TotalNumberOfCodes:  attributes.TotalNumberOfCodes,
				ProductionCodeCount: attributes.ProductionCodeCount,
				SandboxCodeCount:    attributes.SandboxCodeCount, Active: attributes.Active,
				AutoRenewEnabled:       attributes.AutoRenewEnabled,
				TargetSubscriptionPlan: attributes.TargetSubscriptionPlanType,
			})
		}
		next = payload.Links.Next
		if next != "" && !client.allowedNextURL(next) {
			return nil, ErrInvalid
		}
	}
	if next != "" {
		return nil, ErrInvalid
	}
	return offers, nil
}

func (client *Client) CreateFreeOffer(ctx context.Context, draft OfferDraft) (Offer, error) {
	body := map[string]any{"data": map[string]any{
		"type": "subscriptionOfferCodes",
		"attributes": map[string]any{
			"name": draft.Name, "customerEligibilities": draft.CustomerEligibilities,
			"offerEligibility": "REPLACE_INTRO_OFFERS", "duration": draft.Duration,
			"offerMode": "FREE_TRIAL", "numberOfPeriods": 1, "autoRenewEnabled": draft.AutoRenewEnabled,
			"targetSubscriptionPlanType": "MONTHLY",
		},
		"relationships": map[string]any{
			"subscription": map[string]any{"data": map[string]string{"type": "subscriptions", "id": client.config.SubscriptionID}},
			"prices":       map[string]any{"data": []any{}},
		},
	}}
	var response struct {
		Data jsonAPIResource[offerAttributes] `json:"data"`
	}
	if err := client.send(ctx, http.MethodPost, "/v1/subscriptionOfferCodes", body, http.StatusCreated, &response); err != nil {
		return Offer{}, err
	}
	return mapOffer(response.Data)
}

func (client *Client) DeactivateOffer(ctx context.Context, id string) (Offer, error) {
	path := "/v1/subscriptionOfferCodes/" + url.PathEscape(id)
	body := map[string]any{"data": map[string]any{
		"type": "subscriptionOfferCodes", "id": id, "attributes": map[string]bool{"active": false},
	}}
	var response struct {
		Data jsonAPIResource[offerAttributes] `json:"data"`
	}
	if err := client.send(ctx, http.MethodPatch, path, body, http.StatusOK, &response); err != nil {
		return Offer{}, err
	}
	return mapOffer(response.Data)
}

func (client *Client) CreateCustomCode(ctx context.Context, offerID, code string, count int, expirationDate string) (CodePool, error) {
	attributes := map[string]any{"customCode": code, "numberOfCodes": count}
	if expirationDate != "" {
		attributes["expirationDate"] = expirationDate
	}
	body := codePoolCreateBody("subscriptionOfferCodeCustomCodes", offerID, attributes)
	var response struct {
		Data jsonAPIResource[struct {
			CustomCode     string `json:"customCode"`
			NumberOfCodes  int    `json:"numberOfCodes"`
			ExpirationDate string `json:"expirationDate"`
			Active         bool   `json:"active"`
		}] `json:"data"`
	}
	if err := client.send(ctx, http.MethodPost, "/v1/subscriptionOfferCodeCustomCodes", body, http.StatusCreated, &response); err != nil {
		return CodePool{}, err
	}
	return CodePool{ID: response.Data.ID, Kind: "custom", Code: response.Data.Attributes.CustomCode,
		NumberOfCodes: response.Data.Attributes.NumberOfCodes, ExpirationDate: response.Data.Attributes.ExpirationDate,
		Active: response.Data.Attributes.Active}, nil
}

func (client *Client) CreateOneTimeCodeBatch(ctx context.Context, offerID string, count int, expirationDate, environment string) (CodePool, error) {
	attributes := map[string]any{"numberOfCodes": count, "expirationDate": expirationDate, "environment": environment}
	body := codePoolCreateBody("subscriptionOfferCodeOneTimeUseCodes", offerID, attributes)
	var response struct {
		Data jsonAPIResource[struct {
			NumberOfCodes  int    `json:"numberOfCodes"`
			ExpirationDate string `json:"expirationDate"`
			Environment    string `json:"environment"`
			Active         bool   `json:"active"`
		}] `json:"data"`
	}
	if err := client.send(ctx, http.MethodPost, "/v1/subscriptionOfferCodeOneTimeUseCodes", body, http.StatusCreated, &response); err != nil {
		return CodePool{}, err
	}
	return CodePool{ID: response.Data.ID, Kind: "oneTime", NumberOfCodes: response.Data.Attributes.NumberOfCodes,
		ExpirationDate: response.Data.Attributes.ExpirationDate, Environment: response.Data.Attributes.Environment,
		Active: response.Data.Attributes.Active}, nil
}

func (client *Client) ListCodePools(ctx context.Context, offerID string) ([]CodePool, error) {
	customPath := "/v1/subscriptionOfferCodes/" + url.PathEscape(offerID) + "/customCodes"
	oneTimePath := "/v1/subscriptionOfferCodes/" + url.PathEscape(offerID) + "/oneTimeUseCodes"
	custom, err := client.listCustomCodePools(ctx, customPath)
	if err != nil {
		return nil, err
	}
	oneTime, err := client.listOneTimeCodePools(ctx, oneTimePath)
	if err != nil {
		return nil, err
	}
	return append(custom, oneTime...), nil
}

func (client *Client) listCustomCodePools(ctx context.Context, path string) ([]CodePool, error) {
	var response listResponse[struct {
		CustomCode     string `json:"customCode"`
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
		Active         bool   `json:"active"`
	}]
	if err := client.get(ctx, strings.TrimRight(client.config.BaseURL, "/")+path+"?limit=200", "GET "+path, &response); err != nil {
		return nil, err
	}
	pools := make([]CodePool, 0, len(response.Data))
	for _, resource := range response.Data {
		if resource.ID == "" || resource.Type != "subscriptionOfferCodeCustomCodes" {
			return nil, ErrInvalid
		}
		pools = append(pools, CodePool{ID: resource.ID, Kind: "custom", Code: resource.Attributes.CustomCode,
			NumberOfCodes: resource.Attributes.NumberOfCodes, ExpirationDate: resource.Attributes.ExpirationDate, Active: resource.Attributes.Active})
	}
	return pools, nil
}

func (client *Client) listOneTimeCodePools(ctx context.Context, path string) ([]CodePool, error) {
	var response listResponse[struct {
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
		Environment    string `json:"environment"`
		Active         bool   `json:"active"`
	}]
	if err := client.get(ctx, strings.TrimRight(client.config.BaseURL, "/")+path+"?limit=200", "GET "+path, &response); err != nil {
		return nil, err
	}
	pools := make([]CodePool, 0, len(response.Data))
	for _, resource := range response.Data {
		if resource.ID == "" || resource.Type != "subscriptionOfferCodeOneTimeUseCodes" {
			return nil, ErrInvalid
		}
		pools = append(pools, CodePool{ID: resource.ID, Kind: "oneTime", NumberOfCodes: resource.Attributes.NumberOfCodes,
			ExpirationDate: resource.Attributes.ExpirationDate, Environment: resource.Attributes.Environment, Active: resource.Attributes.Active})
	}
	return pools, nil
}

func (client *Client) DownloadOneTimeCodes(ctx context.Context, batchID string) ([]byte, error) {
	path := "/v1/subscriptionOfferCodeOneTimeUseCodes/" + url.PathEscape(batchID) + "/values"
	token, err := client.bearerToken([]string{"GET " + path})
	if err != nil {
		return nil, ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(client.config.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/csv")
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, ErrForbidden
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumCSVBytes+1))
	if err != nil || len(data) == 0 || len(data) > maximumCSVBytes {
		return nil, ErrInvalid
	}
	return data, nil
}

func codePoolCreateBody(resourceType, offerID string, attributes map[string]any) map[string]any {
	return map[string]any{"data": map[string]any{
		"type": resourceType, "attributes": attributes,
		"relationships": map[string]any{"offerCode": map[string]any{"data": map[string]string{
			"type": "subscriptionOfferCodes", "id": offerID,
		}}},
	}}
}

func mapOffer(resource jsonAPIResource[offerAttributes]) (Offer, error) {
	if resource.ID == "" || resource.Type != "subscriptionOfferCodes" || resource.Attributes.Name == "" {
		return Offer{}, ErrInvalid
	}
	attributes := resource.Attributes
	return Offer{ID: resource.ID, Name: attributes.Name, CustomerEligibilities: attributes.CustomerEligibilities,
		OfferEligibility: attributes.OfferEligibility, Duration: attributes.Duration, OfferMode: attributes.OfferMode,
		NumberOfPeriods: attributes.NumberOfPeriods, TotalNumberOfCodes: attributes.TotalNumberOfCodes,
		ProductionCodeCount: attributes.ProductionCodeCount, SandboxCodeCount: attributes.SandboxCodeCount,
		Active: attributes.Active, AutoRenewEnabled: attributes.AutoRenewEnabled,
		TargetSubscriptionPlan: attributes.TargetSubscriptionPlanType}, nil
}

func (client *Client) get(ctx context.Context, endpoint string, scope string, destination any) error {
	token, err := client.bearerToken([]string{scope})
	if err != nil {
		return ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnavailable, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(data) > maximumResponseBytes {
		return ErrInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	return nil
}

func (client *Client) send(ctx context.Context, method, path string, body any, expectedStatus int, destination any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return ErrInvalid
	}
	token, err := client.bearerToken([]string{method + " " + path})
	if err != nil {
		return ErrUnavailable
	}
	endpoint := strings.TrimRight(client.config.BaseURL, "/") + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrForbidden
	}
	if response.StatusCode != expectedStatus {
		return fmt.Errorf("%w: status %d", ErrUnavailable, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(data) > maximumResponseBytes {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	return nil
}

func (client *Client) bearerToken(scope []string) (string, error) {
	now := client.config.Now().UTC()
	header, err := encodeJSONSegment(map[string]any{"alg": "ES256", "kid": client.config.KeyID, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := encodeJSONSegment(map[string]any{
		"iss": client.config.IssuerID, "iat": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
		"aud": "appstoreconnect-v1", "scope": scope,
	})
	if err != nil {
		return "", err
	}
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, client.config.SigningKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := append(fixedWidth(r, 32), fixedWidth(s, 32)...)
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (client *Client) allowedNextURL(value string) bool {
	base, baseErr := url.Parse(client.config.BaseURL)
	next, nextErr := url.Parse(value)
	return baseErr == nil && nextErr == nil && next.Scheme == base.Scheme && next.Host == base.Host &&
		strings.HasPrefix(next.Path, "/v1/subscriptions/"+url.PathEscape(client.config.SubscriptionID)+"/offerCodes")
}

func encodeJSONSegment(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func fixedWidth(value *big.Int, width int) []byte {
	result := make([]byte, width)
	bytes := value.Bytes()
	copy(result[width-len(bytes):], bytes)
	return result
}
