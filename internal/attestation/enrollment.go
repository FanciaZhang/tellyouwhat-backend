package attestation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const registrationChallengeTTL = 5 * time.Minute

var ErrEnrollmentDenied = errors.New("app attest enrollment denied")

type EnrollmentConfig struct {
	Environment       Environment
	DevelopmentSecret string
	AllowedBuilds     map[string]struct{}
}

type Challenge struct {
	Value     string    `json:"challenge"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type RegistrationRequest struct {
	KeyID            string `json:"keyID"`
	Challenge        string `json:"challenge"`
	Attestation      string `json:"attestation"`
	Build            string `json:"build"`
	ActivationSecret string `json:"activationSecret"`
}

type AttestationObjectVerifier interface {
	Verify(string, []byte, []byte) (VerifiedAttestation, error)
}

type EnrollmentKeyStore interface {
	Register(context.Context, RegisteredKey) error
}

type EnrollmentService struct {
	config   EnrollmentConfig
	nonces   NonceStore
	keys     EnrollmentKeyStore
	verifier AttestationObjectVerifier
	now      func() time.Time
}

func NewEnrollmentService(
	config EnrollmentConfig,
	nonces NonceStore,
	keys EnrollmentKeyStore,
	verifier AttestationObjectVerifier,
	now func() time.Time,
) *EnrollmentService {
	if now == nil {
		now = time.Now
	}
	return &EnrollmentService{config: config, nonces: nonces, keys: keys, verifier: verifier, now: now}
}

func (service *EnrollmentService) IssueChallenge(ctx context.Context, keyID string) (Challenge, error) {
	if service == nil || service.nonces == nil || keyID == "" || len(keyID) > 512 {
		return Challenge{}, ErrEnrollmentDenied
	}
	now := service.now()
	value, err := service.nonces.Issue(ctx, keyID, registrationChallengeTTL, now)
	if err != nil {
		return Challenge{}, fmt.Errorf("%w: issue enrollment challenge: %v", ErrUnavailable, err)
	}
	return Challenge{Value: value, ExpiresAt: now.Add(registrationChallengeTTL)}, nil
}

func (service *EnrollmentService) Register(
	ctx context.Context,
	request RegistrationRequest,
) (Principal, error) {
	if service == nil || service.nonces == nil || service.keys == nil || service.verifier == nil ||
		request.KeyID == "" || request.Challenge == "" || request.Attestation == "" {
		return Principal{}, ErrEnrollmentDenied
	}
	if len(service.config.AllowedBuilds) > 0 {
		if _, ok := service.config.AllowedBuilds[request.Build]; !ok {
			return Principal{}, ErrEnrollmentDenied
		}
	}
	if service.config.Environment == EnvironmentDevelopment {
		expected := []byte(service.config.DevelopmentSecret)
		provided := []byte(request.ActivationSecret)
		if len(expected) == 0 || len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			return Principal{}, ErrEnrollmentDenied
		}
	}
	if err := service.nonces.Consume(ctx, request.Challenge, request.KeyID, service.now()); err != nil {
		if errors.Is(err, ErrAuthentication) || errors.Is(err, ErrReplay) {
			return Principal{}, err
		}
		return Principal{}, fmt.Errorf("%w: consume enrollment challenge: %v", ErrUnavailable, err)
	}
	attestationObject, err := decodeBase64(request.Attestation)
	if err != nil {
		return Principal{}, ErrEnrollmentDenied
	}
	clientDataHash := sha256.Sum256([]byte(request.Challenge))
	verified, err := service.verifier.Verify(request.KeyID, attestationObject, clientDataHash[:])
	if err != nil {
		return Principal{}, ErrEnrollmentDenied
	}
	deviceID, err := newDeviceID()
	if err != nil {
		return Principal{}, fmt.Errorf("%w: generate device ID: %v", ErrUnavailable, err)
	}
	key := RegisteredKey{
		KeyID:       request.KeyID,
		DeviceID:    deviceID,
		PublicKey:   verified.PublicKey,
		Counter:     0,
		Environment: string(service.config.Environment),
		Receipt:     verified.Receipt,
	}
	if err := service.keys.Register(ctx, key); err != nil {
		if errors.Is(err, ErrKeyAlreadyRegistered) {
			return Principal{}, err
		}
		return Principal{}, fmt.Errorf("%w: register app attest key: %v", ErrUnavailable, err)
	}
	return Principal{KeyID: key.KeyID, DeviceID: deviceID}, nil
}

func newDeviceID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

