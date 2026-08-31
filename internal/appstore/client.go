// Created by OpenAI Codex on 2026-08-03.

package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	appStoreAudience        = "appstoreconnect-v1"
	appStoreTokenLifetime   = 5 * time.Minute
	maximumAPIResponseBytes = 1 << 20
)

var (
	ErrInvalidAPIResponse  = errors.New("invalid app store server API response")
	ErrTransactionNotFound = errors.New("app store transaction not found")
)

type APIClientConfig struct {
	BaseURL     string
	KeyID       string
	IssuerID    string
	BundleID    string
	AppAppleID  int64
	Environment string
	SigningKey  *ecdsa.PrivateKey
	HTTPClient  *http.Client
	Now         func() time.Time
}

type APIClient struct {
	config APIClientConfig
}

type statusResponse struct {
	Environment string                            `json:"environment"`
	BundleID    string                            `json:"bundleId"`
	AppAppleID  int64                             `json:"appAppleId"`
	Data        []subscriptionGroupIdentifierItem `json:"data"`
}

type subscriptionGroupIdentifierItem struct {
	LastTransactions []lastTransactionItem `json:"lastTransactions"`
}

type lastTransactionItem struct {
	Status                int    `json:"status"`
	SignedTransactionInfo string `json:"signedTransactionInfo"`
	SignedRenewalInfo     string `json:"signedRenewalInfo"`
}

func NewAPIClient(config APIClientConfig) *APIClient {
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &APIClient{config: config}
}

func ParseSigningKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(rest) != 0 || block.Type != "PRIVATE KEY" {
		return nil, ErrStatusUnavailable
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrStatusUnavailable
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, ErrStatusUnavailable
	}
	return key, nil
}

func (client *APIClient) CurrentTransactions(
	ctx context.Context,
	transactionID string,
) ([]SubscriptionStatus, error) {
	if client == nil || !client.validConfig() || transactionID == "" || len(transactionID) > 128 {
		return nil, ErrStatusUnavailable
	}
	bearer, err := client.bearerToken()
	if err != nil {
		return nil, fmt.Errorf("%w: create authorization token", ErrStatusUnavailable)
	}
	endpoint := strings.TrimRight(client.config.BaseURL, "/") +
		"/inApps/v1/subscriptions/" + url.PathEscape(transactionID) + "?status=1&status=4"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create status request", ErrStatusUnavailable)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request current status", ErrStatusUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusNotFound {
		return nil, ErrTransactionNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status code %d", ErrStatusUnavailable, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumAPIResponseBytes+1))
	if err != nil || len(data) > maximumAPIResponseBytes {
		return nil, ErrInvalidAPIResponse
	}
	var payload statusResponse
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, ErrInvalidAPIResponse
	}
	if payload.BundleID != client.config.BundleID ||
		!strings.EqualFold(payload.Environment, client.config.Environment) ||
		(strings.EqualFold(client.config.Environment, "Production") && payload.AppAppleID != client.config.AppAppleID) {
		return nil, ErrInvalidAPIResponse
	}
	statuses := make([]SubscriptionStatus, 0)
	for _, group := range payload.Data {
		for _, transaction := range group.LastTransactions {
			if (transaction.Status != statusActive && transaction.Status != statusGracePeriod) ||
				transaction.SignedTransactionInfo == "" {
				continue
			}
			statuses = append(statuses, SubscriptionStatus{
				Status:            transaction.Status,
				SignedTransaction: transaction.SignedTransactionInfo,
				SignedRenewal:     transaction.SignedRenewalInfo,
			})
		}
	}
	return statuses, nil
}

func (client *APIClient) validConfig() bool {
	return client.config.BaseURL != "" && client.config.KeyID != "" && client.config.IssuerID != "" &&
		client.config.BundleID != "" && client.config.SigningKey != nil &&
		client.config.SigningKey.Curve == elliptic.P256()
}

func (client *APIClient) bearerToken() (string, error) {
	now := client.config.Now()
	header, err := json.Marshal(map[string]string{
		"alg": "ES256",
		"kid": client.config.KeyID,
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"iss": client.config.IssuerID,
		"iat": now.Unix(),
		"exp": now.Add(appStoreTokenLifetime).Unix(),
		"aud": appStoreAudience,
		"bid": client.config.BundleID,
	})
	if err != nil {
		return "", err
	}
	headerSegment := base64.RawURLEncoding.EncodeToString(header)
	payloadSegment := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := headerSegment + "." + payloadSegment
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, client.config.SigningKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	fillPaddedInteger(signature[:32], r)
	fillPaddedInteger(signature[32:], s)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func fillPaddedInteger(destination []byte, value *big.Int) {
	value.FillBytes(destination)
}
