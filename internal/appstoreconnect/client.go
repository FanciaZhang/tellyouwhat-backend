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
	ErrUnavailable      = errors.New("app store connect unavailable")
	ErrForbidden        = errors.New("app store connect operation forbidden")
	ErrInvalid          = errors.New("invalid app store connect response")
	ErrRejected         = errors.New("app store connect rejected request parameters")
	ErrMethodNotAllowed = errors.New("app store connect request method not allowed")
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

// RequestRejection exposes only a recognized field, never Apple's raw error body.
type RequestRejection struct{ Field string }

func (rejection *RequestRejection) Error() string { return ErrRejected.Error() }
func (rejection *RequestRejection) Unwrap() error { return ErrRejected }

func requestRejection(reader io.Reader) error {
	var response struct {
		Errors []struct {
			Source struct {
				Pointer string `json:"pointer"`
			} `json:"source"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, maximumResponseBytes)).Decode(&response); err == nil {
		for _, failure := range response.Errors {
			switch failure.Source.Pointer {
			case "/data/attributes/numberOfCodes", "/data/attributes/expirationDate", "/data/attributes/customCode":
				return &RequestRejection{Field: strings.TrimPrefix(failure.Source.Pointer, "/data/attributes/")}
			}
		}
	}
	return ErrRejected
}

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
	territories, err := client.offerTerritories(ctx)
	if err != nil {
		return Offer{}, err
	}
	prices := make([]any, 0, len(territories))
	linkages := make([]any, 0, len(territories))
	for index, territory := range territories {
		id := fmt.Sprintf("${price-%d}", index)
		linkages = append(linkages, map[string]string{"type": "subscriptionOfferCodePrices", "id": id})
		prices = append(prices, map[string]any{
			"type": "subscriptionOfferCodePrices", "id": id,
			"relationships": map[string]any{"territory": map[string]any{
				"data": map[string]string{"type": "territories", "id": territory},
			}},
		})
	}
	body := map[string]any{"data": map[string]any{
		"type": "subscriptionOfferCodes",
		"attributes": map[string]any{
			"name": draft.Name, "customerEligibilities": draft.CustomerEligibilities,
			"offerEligibility": "REPLACE_INTRO_OFFERS", "duration": draft.Duration,
			"offerMode": "FREE_TRIAL", "numberOfPeriods": 1, "autoRenewEnabled": draft.AutoRenewEnabled,
			"targetSubscriptionPlanType": "UPFRONT",
		},
		"relationships": map[string]any{
			"subscription": map[string]any{"data": map[string]string{"type": "subscriptions", "id": client.config.SubscriptionID}},
			"prices":       map[string]any{"data": linkages},
		},
	}, "included": prices}
	var response struct {
		Data jsonAPIResource[offerAttributes] `json:"data"`
	}
	if err := client.send(ctx, http.MethodPost, "/v1/subscriptionOfferCodes", body, http.StatusCreated, &response); err != nil {
		return Offer{}, err
	}
	return mapOffer(response.Data)
}

// Standard subscriptions use UPFRONT; MONTHLY is a twelve-month commitment plan.
func (client *Client) offerTerritories(ctx context.Context) ([]string, error) {
	base := strings.TrimRight(client.config.BaseURL, "/")
	path := "/v1/subscriptions/" + url.PathEscape(client.config.SubscriptionID) + "/planAvailabilities"
	var plans listResponse[struct {
		PlanType string `json:"planType"`
	}]
	if err := client.get(ctx, base+path+"?limit=200", "GET "+path, &plans); err != nil {
		return nil, err
	}
	planID := ""
	for _, plan := range plans.Data {
		if plan.Attributes.PlanType == "UPFRONT" {
			if planID != "" || plan.ID == "" || plan.Type != "subscriptionPlanAvailabilities" {
				return nil, ErrInvalid
			}
			planID = plan.ID
		}
	}
	if planID == "" || plans.Links.Next != "" {
		return nil, ErrInvalid
	}
	path = "/v1/subscriptionPlanAvailabilities/" + url.PathEscape(planID) + "/availableTerritories"
	next := base + path + "?limit=200"
	var territories []string
	seen := map[string]bool{}
	for page := 0; page < maximumPaginationPages && next != ""; page++ {
		var payload listResponse[struct{}]
		if err := client.get(ctx, next, "GET "+path, &payload); err != nil {
			return nil, err
		}
		for _, territory := range payload.Data {
			if territory.Type != "territories" || territory.ID == "" || seen[territory.ID] {
				return nil, ErrInvalid
			}
			seen[territory.ID] = true
			territories = append(territories, territory.ID)
		}
		next = payload.Links.Next
		if next != "" {
			expected, _ := url.Parse(base + path)
			actual, err := url.Parse(next)
			if err != nil || actual.Scheme != expected.Scheme || actual.Host != expected.Host || actual.Path != expected.Path || actual.User != nil {
				return nil, ErrInvalid
			}
		}
	}
	if next != "" || len(territories) == 0 {
		return nil, ErrInvalid
	}
	return territories, nil
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
	resources, err := listPoolResources[struct {
		CustomCode     string `json:"customCode"`
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
		Active         bool   `json:"active"`
	}](ctx, client, path)
	if err != nil {
		return nil, err
	}
	pools := make([]CodePool, 0, len(resources))
	for _, resource := range resources {
		if resource.ID == "" || resource.Type != "subscriptionOfferCodeCustomCodes" {
			return nil, ErrInvalid
		}
		pools = append(pools, CodePool{ID: resource.ID, Kind: "custom", Code: resource.Attributes.CustomCode,
			NumberOfCodes: resource.Attributes.NumberOfCodes, ExpirationDate: resource.Attributes.ExpirationDate, Active: resource.Attributes.Active})
	}
	return pools, nil
}

func (client *Client) listOneTimeCodePools(ctx context.Context, path string) ([]CodePool, error) {
	resources, err := listPoolResources[struct {
		NumberOfCodes  int    `json:"numberOfCodes"`
		ExpirationDate string `json:"expirationDate"`
		Environment    string `json:"environment"`
		Active         bool   `json:"active"`
	}](ctx, client, path)
	if err != nil {
		return nil, err
	}
	pools := make([]CodePool, 0, len(resources))
	for _, resource := range resources {
		if resource.ID == "" || resource.Type != "subscriptionOfferCodeOneTimeUseCodes" {
			return nil, ErrInvalid
		}
		pools = append(pools, CodePool{ID: resource.ID, Kind: "oneTime", NumberOfCodes: resource.Attributes.NumberOfCodes,
			ExpirationDate: resource.Attributes.ExpirationDate, Environment: resource.Attributes.Environment, Active: resource.Attributes.Active})
	}
	return pools, nil
}

// listPoolResources requires complete pagination and rejects cross-resource continuation links.
func listPoolResources[T any](ctx context.Context, client *Client, path string) ([]jsonAPIResource[T], error) {
	next := strings.TrimRight(client.config.BaseURL, "/") + path + "?limit=200"
	base, _ := url.Parse(client.config.BaseURL)
	out := []jsonAPIResource[T]{}
	seenPages := map[string]bool{}
	seenIDs := map[string]bool{}
	for page := 0; next != "" && page < maximumPaginationPages; page++ {
		if seenPages[next] {
			return nil, ErrInvalid
		}
		seenPages[next] = true
		var payload listResponse[T]
		if err := client.get(ctx, next, "GET "+path, &payload); err != nil {
			return nil, err
		}
		for _, resource := range payload.Data {
			if seenIDs[resource.ID] {
				return nil, ErrInvalid
			}
			seenIDs[resource.ID] = true
			out = append(out, resource)
		}
		next = payload.Links.Next
		if next != "" {
			u, err := url.Parse(next)
			if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.Path != path || u.User != nil || u.Fragment != "" {
				return nil, ErrInvalid
			}
		}
	}
	if next != "" {
		return nil, ErrInvalid
	}
	return out, nil
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
	// Apple accepts scope entries only for GET requests. Writes use the existing
	// API key role and a short-lived token without a scope claim.
	token, err := client.bearerToken(nil)
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
		switch response.StatusCode {
		case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
			return requestRejection(response.Body)
		case http.StatusMethodNotAllowed:
			return ErrMethodNotAllowed
		}
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
	claims := map[string]any{
		"iss": client.config.IssuerID, "iat": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
		"aud": "appstoreconnect-v1",
	}
	if len(scope) > 0 {
		claims["scope"] = scope
	}
	payload, err := encodeJSONSegment(claims)
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
