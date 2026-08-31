package attestation

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

const defaultTimestampWindow = 5 * time.Minute

var (
	ErrAuthentication             = errors.New("app attest authentication failed")
	ErrReplay                     = errors.New("app attest replay detected")
	ErrKeyNotFound                = errors.New("app attest key not found")
	ErrKeyAlreadyRegistered       = errors.New("app attest key already registered")
	ErrUnavailable                = errors.New("app attest infrastructure unavailable")
	ErrTransactionBindingConflict = errors.New("app attest key transaction binding conflict")
)

type RequestProof struct {
	Method     string
	Path       string
	RequestID  string
	KeyID      string
	Assertion  string
	Nonce      string
	Timestamp  string
	BodySHA256 string
}

type Principal struct {
	KeyID         string
	DeviceID      string
	TransactionID string
}

type RegisteredKey struct {
	KeyID         string
	DeviceID      string
	TransactionID string
	PublicKey     []byte
	Counter       uint32
	Environment   string
	Receipt       []byte
}

type NonceStore interface {
	Issue(context.Context, string, time.Duration, time.Time) (string, error)
	Consume(context.Context, string, string, time.Time) error
}

type KeyStore interface {
	Get(context.Context, string) (RegisteredKey, error)
	AdvanceCounter(context.Context, string, uint32, uint32) error
}

type AssertionVerifier interface {
	VerifyAssertion(publicKey, assertion, clientDataHash []byte) (uint32, error)
}

type Service struct {
	nonces              NonceStore
	keys                KeyStore
	verifier            AssertionVerifier
	now                 func() time.Time
	timestampWindow     time.Duration
	expectedEnvironment string
}

func (service *Service) RequireEnvironment(environment Environment) *Service {
	service.expectedEnvironment = string(environment)
	return service
}

func NewService(
	nonces NonceStore,
	keys KeyStore,
	verifier AssertionVerifier,
	now func() time.Time,
) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		nonces:          nonces,
		keys:            keys,
		verifier:        verifier,
		now:             now,
		timestampWindow: defaultTimestampWindow,
	}
}

func (service *Service) Authenticate(ctx context.Context, proof RequestProof) (Principal, error) {
	if service == nil || service.nonces == nil || service.keys == nil || service.verifier == nil {
		return Principal{}, ErrAuthentication
	}
	if proof.Method == "" || proof.Path == "" || proof.RequestID == "" || proof.KeyID == "" ||
		proof.Assertion == "" || proof.Nonce == "" || proof.Timestamp == "" || proof.BodySHA256 == "" {
		return Principal{}, ErrAuthentication
	}
	timestamp, err := time.Parse(time.RFC3339, proof.Timestamp)
	if err != nil || math.Abs(service.now().Sub(timestamp).Seconds()) > service.timestampWindow.Seconds() {
		return Principal{}, ErrAuthentication
	}
	key, err := service.keys.Get(ctx, proof.KeyID)
	if errors.Is(err, ErrKeyNotFound) {
		return Principal{}, ErrAuthentication
	}
	if err != nil {
		return Principal{}, fmt.Errorf("%w: load registered key: %v", ErrUnavailable, err)
	}
	if service.expectedEnvironment != "" && key.Environment != service.expectedEnvironment {
		return Principal{}, ErrAuthentication
	}
	if err := service.nonces.Consume(ctx, proof.Nonce, proof.KeyID, service.now()); err != nil {
		if errors.Is(err, ErrAuthentication) || errors.Is(err, ErrReplay) {
			return Principal{}, err
		}
		return Principal{}, fmt.Errorf("%w: consume nonce: %v", ErrUnavailable, err)
	}
	assertion, err := decodeBase64(proof.Assertion)
	if err != nil {
		return Principal{}, ErrAuthentication
	}
	digestHex := contracts.RequestBindingDigest(contracts.RequestBinding{
		Method:     proof.Method,
		Path:       proof.Path,
		RequestID:  proof.RequestID,
		Nonce:      proof.Nonce,
		Timestamp:  proof.Timestamp,
		BodySHA256: proof.BodySHA256,
	})
	clientDataHash, err := hex.DecodeString(digestHex)
	if err != nil {
		return Principal{}, ErrAuthentication
	}
	counter, err := service.verifier.VerifyAssertion(key.PublicKey, assertion, clientDataHash)
	if err != nil {
		return Principal{}, ErrAuthentication
	}
	if counter <= key.Counter {
		return Principal{}, ErrReplay
	}
	if err := service.keys.AdvanceCounter(ctx, key.KeyID, key.Counter, counter); err != nil {
		if errors.Is(err, ErrReplay) {
			return Principal{}, ErrReplay
		}
		return Principal{}, fmt.Errorf("%w: advance assertion counter: %v", ErrUnavailable, err)
	}
	return Principal{
		KeyID:         key.KeyID,
		DeviceID:      key.DeviceID,
		TransactionID: key.TransactionID,
	}, nil
}

func decodeBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, ErrAuthentication
}

