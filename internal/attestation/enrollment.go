package attestation

import (
	"bytes"
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
	AppID             string
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
	Get(context.Context, string) (RegisteredKey, error)
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
	if service == nil || service.nonces == nil {
		return Challenge{}, diagnosticFailure(stageDependencies, ErrUnavailable)
	}
	if keyID == "" || len(keyID) > 512 {
		return Challenge{}, diagnosticFailure(stageEnrollmentPolicy, ErrEnrollmentDenied)
	}
	now := service.now()
	value, err := service.nonces.Issue(ctx, keyID, registrationChallengeTTL, now)
	if err != nil {
		return Challenge{}, diagnosticFailure(stageNonceIssue, fmt.Errorf("%w: issue enrollment challenge: %v", ErrUnavailable, err))
	}
	return Challenge{Value: value, ExpiresAt: now.Add(registrationChallengeTTL)}, nil
}

func (service *EnrollmentService) Register(
	ctx context.Context,
	request RegistrationRequest,
) (Principal, error) {
	if service == nil || service.config.AppID == "" || service.nonces == nil || service.keys == nil || service.verifier == nil {
		return Principal{}, diagnosticFailure(stageDependencies, ErrUnavailable)
	}
	if request.KeyID == "" || request.Challenge == "" || request.Attestation == "" {
		return Principal{}, diagnosticFailure(stageProofHeaders, ErrEnrollmentDenied)
	}
	if len(service.config.AllowedBuilds) > 0 {
		if _, ok := service.config.AllowedBuilds[request.Build]; !ok {
			return Principal{}, diagnosticFailure(stageEnrollmentPolicy, ErrEnrollmentDenied)
		}
	}
	if service.config.Environment == EnvironmentDevelopment {
		expected := []byte(service.config.DevelopmentSecret)
		provided := []byte(request.ActivationSecret)
		if len(expected) == 0 || len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
			return Principal{}, diagnosticFailure(stageEnrollmentPolicy, ErrEnrollmentDenied)
		}
	}
	nonceErr := service.nonces.Consume(ctx, request.Challenge, request.KeyID, service.now())
	if nonceErr != nil {
		if !errors.Is(nonceErr, ErrReplay) && !errors.Is(nonceErr, ErrAuthentication) {
			return Principal{}, diagnosticFailure(stageNonce, fmt.Errorf("%w: consume enrollment challenge: %v", ErrUnavailable, nonceErr))
		}
		key, found, err := service.registeredKey(ctx, request.KeyID)
		if err != nil {
			return Principal{}, err
		}
		if !found {
			return Principal{}, diagnosticFailure(stageNonce, nonceErr)
		}
		// Recovery only returns an existing identity. Revalidate the original
		// challenge-bound proof and pinned public key; never enroll an expired key.
		verified, err := service.verifyRegistration(request)
		if err != nil {
			// An expired cached proof can also outlive its certificate. Keep the
			// shipped client's bounded fresh-enrollment fallback in that case.
			if errors.Is(nonceErr, ErrAuthentication) {
				return Principal{}, diagnosticFailure(failureStageOf(err), ErrAuthentication)
			}
			return Principal{}, err
		}
		return service.existingPrincipal(key, verified.PublicKey)
	}
	verified, err := service.verifyRegistration(request)
	if err != nil {
		return Principal{}, err
	}
	deviceID, err := newDeviceID()
	if err != nil {
		return Principal{}, diagnosticFailure(stageRegistrationStorage, fmt.Errorf("%w: generate device ID: %v", ErrUnavailable, err))
	}
	key := RegisteredKey{
		AppID:       service.config.AppID,
		KeyID:       request.KeyID,
		DeviceID:    deviceID,
		PublicKey:   verified.PublicKey,
		Counter:     0,
		Environment: string(service.config.Environment),
		Receipt:     verified.Receipt,
	}
	if err := service.keys.Register(ctx, key); err != nil {
		if errors.Is(err, ErrKeyAlreadyRegistered) {
			stored, found, lookupErr := service.registeredKey(ctx, request.KeyID)
			if lookupErr != nil {
				return Principal{}, lookupErr
			}
			if found {
				return service.existingPrincipal(stored, verified.PublicKey)
			}
			return Principal{}, diagnosticFailure(stageRegistrationStorage, fmt.Errorf("%w: registered app attest key could not be read", ErrUnavailable))
		}
		return Principal{}, diagnosticFailure(stageRegistrationStorage, fmt.Errorf("%w: register app attest key: %v", ErrUnavailable, err))
	}
	return Principal{AppID: key.AppID, KeyID: key.KeyID, DeviceID: deviceID}, nil
}

func (service *EnrollmentService) verifyRegistration(request RegistrationRequest) (VerifiedAttestation, error) {
	object, err := decodeBase64(request.Attestation)
	if err != nil {
		return VerifiedAttestation{}, diagnosticFailure(stageAttestationEncoding, ErrEnrollmentDenied)
	}
	hash := sha256.Sum256([]byte(request.Challenge))
	verified, err := service.verifier.Verify(request.KeyID, object, hash[:])
	if err != nil {
		return VerifiedAttestation{}, diagnosticFailure(stageAttestationVerification, ErrEnrollmentDenied)
	}
	return verified, nil
}

func (service *EnrollmentService) registeredKey(ctx context.Context, keyID string) (RegisteredKey, bool, error) {
	key, err := service.keys.Get(ctx, keyID)
	if errors.Is(err, ErrKeyNotFound) {
		return RegisteredKey{}, false, nil
	}
	if err != nil {
		return RegisteredKey{}, false, diagnosticFailure(stageKeyLookup, fmt.Errorf("%w: read registered app attest key: %v", ErrUnavailable, err))
	}
	if key.KeyID != keyID || key.DeviceID == "" {
		return RegisteredKey{}, false, diagnosticFailure(stageRegistrationIdentity, fmt.Errorf("%w: registered app attest key is invalid", ErrUnavailable))
	}
	return key, true, nil
}

func (service *EnrollmentService) existingPrincipal(key RegisteredKey, publicKey []byte) (Principal, error) {
	if key.AppID != service.config.AppID || key.Environment != string(service.config.Environment) ||
		len(publicKey) == 0 || !bytes.Equal(key.PublicKey, publicKey) {
		return Principal{}, diagnosticFailure(stageRegistrationIdentity, ErrEnrollmentDenied)
	}
	return Principal{AppID: key.AppID, KeyID: key.KeyID, DeviceID: key.DeviceID, TransactionID: key.TransactionID}, nil
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
