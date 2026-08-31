package capability

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
)

var (
	ErrInvalid = errors.New("invalid job capability")
	ErrExpired = errors.New("expired job capability")
	ErrReplay  = errors.New("replayed job capability")
)

const maximumLifetime = 24 * time.Hour

type Binding struct {
	JobID       string              `json:"jobID"`
	RequestID   string              `json:"requestID"`
	Operation   contracts.Operation `json:"operation"`
	BodyDigest  string              `json:"bodyDigest"`
	MediaDigest string              `json:"mediaDigest"`
}

type Issued struct {
	JobID     string    `json:"jobID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type UseStore interface {
	Consume(context.Context, string, time.Time, time.Time) (bool, error)
}

type Service struct {
	secret []byte
	uses   UseStore
	now    func() time.Time
	ttl    time.Duration
}

type claims struct {
	Principal attestation.Principal `json:"principal"`
	Binding   Binding               `json:"binding"`
	Nonce     string                `json:"nonce"`
	ExpiresAt time.Time             `json:"expiresAt"`
}

func NewService(secret []byte, uses UseStore, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{secret: append([]byte(nil), secret...), uses: uses, now: now, ttl: maximumLifetime}
}

func (service *Service) Issue(principal attestation.Principal, binding Binding) (Issued, error) {
	return service.IssueAt(principal, binding, service.now().UTC().Truncate(time.Microsecond))
}

func (service *Service) IssueAt(principal attestation.Principal, binding Binding, issuedAt time.Time) (Issued, error) {
	if service == nil || len(service.secret) < 32 || service.uses == nil || principal.AppID == "" || principal.KeyID == "" || principal.DeviceID == "" {
		return Issued{}, ErrInvalid
	}
	if !validBinding(binding, false) {
		return Issued{}, ErrInvalid
	}
	issuedAt = issuedAt.UTC().Truncate(time.Microsecond)
	now := service.now()
	if issuedAt.IsZero() || issuedAt.After(now.Add(5*time.Minute)) {
		return Issued{}, ErrInvalid
	}
	expiresAt := issuedAt.Add(min(service.ttl, maximumLifetime))
	if !now.Before(expiresAt) {
		return Issued{}, ErrExpired
	}
	jobID, err := service.derivedIdentifier("job", principal, binding)
	if err != nil {
		return Issued{}, err
	}
	binding.JobID = jobID
	value := claims{
		Principal: principal,
		Binding:   binding,
		ExpiresAt: expiresAt,
	}
	value.Nonce, err = service.derivedIdentifier("nonce", principal, binding)
	if err != nil {
		return Issued{}, err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return Issued{}, ErrInvalid
	}
	signature := service.sign(payload)
	return Issued{
		JobID:     binding.JobID,
		Token:     base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature),
		ExpiresAt: value.ExpiresAt,
	}, nil
}

func (service *Service) Consume(ctx context.Context, token string, expected Binding) (attestation.Principal, error) {
	value, now, err := service.validate(token, expected)
	if err != nil {
		return attestation.Principal{}, err
	}
	consumed, err := service.uses.Consume(ctx, value.Nonce, value.ExpiresAt, now)
	if err != nil {
		return attestation.Principal{}, err
	}
	if !consumed {
		return attestation.Principal{}, ErrReplay
	}
	return value.Principal, nil
}

// Validate authenticates and binds a capability without consuming its nonce.
// The gateway uses this before its durable job transaction so a transient
// database failure cannot destroy the client's only retry credential.
func (service *Service) Validate(token string, expected Binding) (attestation.Principal, error) {
	value, _, err := service.validate(token, expected)
	if err != nil {
		return attestation.Principal{}, err
	}
	return value.Principal, nil
}

func (service *Service) validate(token string, expected Binding) (claims, time.Time, error) {
	if service == nil || len(service.secret) < 32 || service.uses == nil || !validBinding(expected, true) {
		return claims{}, time.Time{}, ErrInvalid
	}
	payload, signature, ok := splitToken(token)
	if !ok || subtle.ConstantTimeCompare(signature, service.sign(payload)) != 1 {
		return claims{}, time.Time{}, ErrInvalid
	}
	var value claims
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || !validClaims(value) || value.Binding != expected {
		return claims{}, time.Time{}, ErrInvalid
	}
	now := service.now()
	if !now.Before(value.ExpiresAt) || value.ExpiresAt.After(now.Add(maximumLifetime)) {
		return claims{}, time.Time{}, ErrExpired
	}
	return value, now, nil
}

func validClaims(value claims) bool {
	return value.Principal.AppID != "" && value.Principal.KeyID != "" && value.Principal.DeviceID != "" && value.Nonce != "" && validBinding(value.Binding, true)
}

func validBinding(value Binding, requireJobID bool) bool {
	if requireJobID && !contracts.ValidRequestID(value.JobID) {
		return false
	}
	if !contracts.ValidRequestID(value.RequestID) || value.BodyDigest == "" || value.MediaDigest == "" {
		return false
	}
	_, supported := contracts.PolicyFor(value.Operation)
	return supported
}

func (service *Service) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, service.secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func splitToken(token string) ([]byte, []byte, bool) {
	separator := -1
	for index := range token {
		if token[index] == '.' {
			if separator >= 0 {
				return nil, nil, false
			}
			separator = index
		}
	}
	if separator <= 0 || separator == len(token)-1 {
		return nil, nil, false
	}
	payload, payloadErr := base64.RawURLEncoding.DecodeString(token[:separator])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(token[separator+1:])
	return payload, signature, payloadErr == nil && signatureErr == nil && len(signature) == sha256.Size
}

func (service *Service) derivedIdentifier(label string, principal attestation.Principal, binding Binding) (string, error) {
	payload, err := json.Marshal(struct {
		Label     string                `json:"label"`
		Principal attestation.Principal `json:"principal"`
		Binding   Binding               `json:"binding"`
	}{Label: label, Principal: principal, Binding: binding})
	if err != nil {
		return "", ErrInvalid
	}
	mac := hmac.New(sha256.New, service.secret)
	_, _ = mac.Write(payload)
	value := append([]byte(nil), mac.Sum(nil)[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	// Convert the 16 bytes to a canonical UUID without adding another dependency.
	hex := make([]byte, 32)
	const alphabet = "0123456789abcdef"
	for index, item := range value {
		hex[index*2] = alphabet[item>>4]
		hex[index*2+1] = alphabet[item&0x0f]
	}
	return string(hex[0:8]) + "-" + string(hex[8:12]) + "-" + string(hex[12:16]) + "-" + string(hex[16:20]) + "-" + string(hex[20:32]), nil
}
