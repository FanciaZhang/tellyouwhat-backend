package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
)

func TestPrivacyDeletionRecoversLostSuccessWithOriginalProof(t *testing.T) {
	t.Parallel()
	fixture := newPrivacyDeletionRecoveryFixture(t)
	request := fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 1)
	retry := request.Clone(context.Background())
	transport := &lostPrivacyDeletionResponse{base: http.DefaultTransport}
	client := &http.Client{Transport: transport}
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("expected the successful response to be lost before the client receives it")
	}
	fixture.repository.mu.Lock()
	remainingKeys := len(fixture.repository.keys)
	remainingConsents := len(fixture.repository.consents)
	deletions := fixture.repository.deletions
	fixture.repository.mu.Unlock()
	if remainingKeys != 0 || remainingConsents != 0 || deletions != 1 {
		t.Fatalf("first request must complete deletion: keys=%d consents=%d deletions=%d", remainingKeys, remainingConsents, deletions)
	}
	response, err := client.Do(retry)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("lost deletion success must be recoverable with its original proof: got %d %s", response.StatusCode, body)
	}
}

func TestPrivacyDeletionDoesNotAuthorizeFreshProofAfterIdentityRemoval(t *testing.T) {
	t.Parallel()
	fixture := newPrivacyDeletionRecoveryFixture(t)
	first, err := http.DefaultClient.Do(fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 1))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("initial deletion failed: %d", first.StatusCode)
	}
	response, err := http.DefaultClient.Do(fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 2))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a deleted identity cannot authorize a fresh request: %d", response.StatusCode)
	}
}

type privacyDeletionRecoveryFixture struct {
	repository *privacyDeletionRecoveryRepository
	nonces     *attestation.MemoryNonceStore
	privateKey *ecdsa.PrivateKey
	server     *httptest.Server
	gateway    *Server
	now        time.Time
}

func newPrivacyDeletionRecoveryFixture(t *testing.T) *privacyDeletionRecoveryFixture {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	repository := &privacyDeletionRecoveryRepository{
		receipts: make(map[privacy.DeletionReceipt]bool),
		keys: map[string]attestation.RegisteredKey{"synthetic-deletion-key": {
			AppID: "health", KeyID: "synthetic-deletion-key", DeviceID: "synthetic-deletion-device", PublicKey: publicKey,
		}},
		consents: []privacy.Record{{KeyID: "synthetic-deletion-key", DeviceID: "synthetic-deletion-device", Scope: privacy.PrivacyTermsScope}},
	}
	nonces := attestation.NewMemoryNonceStore()
	now := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	authenticator := attestation.NewService(nonces, repository,
		attestation.NewAppleAssertionVerifier("SYNTHETIC", "cn.tellyouwhat.healthapp"), clock)
	server := New(Dependencies{
		Authenticator: authenticator,
		Privacy:       privacy.NewService(repository, privacyDeletionNoObjects{}, nil, clock),
		Now:           clock,
	})
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	return &privacyDeletionRecoveryFixture{repository: repository, nonces: nonces, privateKey: privateKey, server: httpServer, gateway: server, now: now}
}

func (fixture *privacyDeletionRecoveryFixture) signedRequest(t *testing.T, method, path string, counter uint32) *http.Request {
	t.Helper()
	nonce, err := fixture.nonces.Issue(context.Background(), "synthetic-deletion-key", time.Minute, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "ce6f0b5b-11e2-45e2-a0c1-83fc06c01128"
	timestamp := fixture.now.Format(time.RFC3339)
	digest, err := hex.DecodeString(contracts.RequestBindingDigest(contracts.RequestBinding{
		Method: method, Path: path, RequestID: requestID, Nonce: nonce, Timestamp: timestamp,
		BodySHA256: contracts.BodySHA256(nil),
	}))
	if err != nil {
		t.Fatal(err)
	}
	authenticatorData := make([]byte, 37)
	rpIDHash := sha256.Sum256([]byte("SYNTHETIC.cn.tellyouwhat.healthapp"))
	copy(authenticatorData, rpIDHash[:])
	authenticatorData[32] = 0x01
	binary.BigEndian.PutUint32(authenticatorData[33:], counter)
	assertionNonce := sha256.Sum256(append(append([]byte(nil), authenticatorData...), digest...))
	signedDigest := sha256.Sum256(assertionNonce[:])
	signature, err := ecdsa.SignASN1(rand.Reader, fixture.privateKey, signedDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := cbor.Marshal(map[string]any{"authenticatorData": authenticatorData, "signature": signature})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, fixture.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Tellyouwhat-Key-ID", "synthetic-deletion-key")
	request.Header.Set("X-Tellyouwhat-Assertion", base64.StdEncoding.EncodeToString(assertion))
	request.Header.Set("X-Tellyouwhat-Nonce", nonce)
	request.Header.Set("X-Tellyouwhat-Timestamp", timestamp)
	request.Header.Set("X-Tellyouwhat-Request-ID", requestID)
	return request
}

type lostPrivacyDeletionResponse struct {
	base    http.RoundTripper
	dropped bool
}

func (transport *lostPrivacyDeletionResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil || transport.dropped || response.StatusCode != http.StatusNoContent {
		return response, err
	}
	transport.dropped = true
	response.Body.Close()
	return nil, io.ErrUnexpectedEOF
}

type privacyDeletionRecoveryRepository struct {
	mu          sync.Mutex
	keys        map[string]attestation.RegisteredKey
	consents    []privacy.Record
	deletions   int
	receipts    map[privacy.DeletionReceipt]bool
	lookupError error
	writeError  error
}

func (repository *privacyDeletionRecoveryRepository) Get(_ context.Context, keyID string) (attestation.RegisteredKey, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key, exists := repository.keys[keyID]
	if !exists {
		return attestation.RegisteredKey{}, attestation.ErrKeyNotFound
	}
	return key, nil
}

func (repository *privacyDeletionRecoveryRepository) AdvanceCounter(_ context.Context, keyID string, expected, next uint32) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	key, exists := repository.keys[keyID]
	if !exists {
		return attestation.ErrKeyNotFound
	}
	if key.Counter != expected || next <= expected {
		return attestation.ErrReplay
	}
	key.Counter = next
	repository.keys[keyID] = key
	return nil
}

func (repository *privacyDeletionRecoveryRepository) RecordConsents(_ context.Context, values []privacy.Record) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.consents = append(repository.consents, values...)
	return nil
}

func (repository *privacyDeletionRecoveryRepository) PlanDeletion(_ context.Context, principal attestation.Principal) (privacy.DeletionPlan, error) {
	return privacy.DeletionPlan{Principals: []attestation.Principal{principal}}, nil
}

func (repository *privacyDeletionRecoveryRepository) DeletePrincipal(_ context.Context, principal attestation.Principal) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.deletePrincipal(principal)
	return nil
}

func (repository *privacyDeletionRecoveryRepository) deletePrincipal(principal attestation.Principal) {
	delete(repository.keys, principal.KeyID)
	remaining := repository.consents[:0]
	for _, consent := range repository.consents {
		if consent.KeyID != principal.KeyID {
			remaining = append(remaining, consent)
		}
	}
	repository.consents = remaining
	repository.deletions++
}

func (repository *privacyDeletionRecoveryRepository) DeletionCompleted(_ context.Context, receipt privacy.DeletionReceipt) (bool, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.receipts[receipt], repository.lookupError
}

func (repository *privacyDeletionRecoveryRepository) DeletePrincipalWithReceipt(_ context.Context, principal attestation.Principal, receipt privacy.DeletionReceipt) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.writeError != nil {
		return repository.writeError
	}
	repository.deletePrincipal(principal)
	repository.receipts[receipt] = true
	return nil
}

type privacyDeletionNoObjects struct{}

func (privacyDeletionNoObjects) DeleteObject(context.Context, string) error {
	return errors.New("no objects are present in this fixture")
}

func TestPrivacyDeletionReceiptRejectsChangedProofAndOtherApplications(t *testing.T) {
	t.Parallel()
	fixture := newPrivacyDeletionRecoveryFixture(t)
	original := fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 1)
	assertPrivacyHTTPStatus(t, original.Clone(context.Background()), http.StatusNoContent)
	for header, value := range map[string]string{
		"X-Tellyouwhat-Key-ID": "another-key", "X-Tellyouwhat-Assertion": "another-assertion",
		"X-Tellyouwhat-Nonce": "another-nonce", "X-Tellyouwhat-Timestamp": "2026-09-06T06:00:01Z",
		"X-Tellyouwhat-Request-ID": "ce6f0b5b-11e2-45e2-a0c1-83fc06c01129",
	} {
		t.Run(header, func(t *testing.T) {
			request := original.Clone(context.Background())
			request.Header.Set(header, value)
			assertPrivacyHTTPStatus(t, request, http.StatusUnauthorized)
		})
	}
	changedBody := original.Clone(context.Background())
	changedBody.Body = io.NopCloser(strings.NewReader("different"))
	changedBody.ContentLength = int64(len("different"))
	assertPrivacyHTTPStatus(t, changedBody, http.StatusUnprocessableEntity)
	journal := New(Dependencies{App: appregistry.App{ID: appregistry.Journal}, Authenticator: fixture.gateway.authenticator, Privacy: fixture.gateway.privacy})
	otherServer := httptest.NewServer(journal.Router())
	defer otherServer.Close()
	otherRequest := original.Clone(context.Background())
	otherRequest.URL.Host = strings.TrimPrefix(otherServer.URL, "http://")
	assertPrivacyHTTPStatus(t, otherRequest, http.StatusUnauthorized)
	assertPrivacyHTTPStatus(t, original.Clone(context.Background()), http.StatusNoContent)
}

func TestPrivacyDeletionReceiptSurvivesServerRestartAndExpiredNonce(t *testing.T) {
	t.Parallel()
	fixture := newPrivacyDeletionRecoveryFixture(t)
	original := fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 1)
	assertPrivacyHTTPStatus(t, original.Clone(context.Background()), http.StatusNoContent)
	future := func() time.Time { return fixture.now.Add(30 * 24 * time.Hour) }
	server := New(Dependencies{
		Authenticator: attestation.NewService(fixture.nonces, fixture.repository, attestation.NewAppleAssertionVerifier("SYNTHETIC", "cn.tellyouwhat.healthapp"), future),
		Privacy:       privacy.NewService(fixture.repository, privacyDeletionNoObjects{}, nil, future), Now: future,
	})
	restarted := httptest.NewServer(server.Router())
	defer restarted.Close()
	original.URL.Host = strings.TrimPrefix(restarted.URL, "http://")
	assertPrivacyHTTPStatus(t, original, http.StatusNoContent)
}

func TestPrivacyDeletionStorageFailureNeverReportsCompletion(t *testing.T) {
	t.Parallel()
	fixture := newPrivacyDeletionRecoveryFixture(t)
	fixture.repository.lookupError = errors.New("synthetic lookup outage")
	request := fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 1)
	assertPrivacyHTTPStatus(t, request.Clone(context.Background()), http.StatusServiceUnavailable)
	fixture.repository.mu.Lock()
	fixture.repository.lookupError = nil
	fixture.repository.writeError = errors.New("synthetic write outage")
	fixture.repository.mu.Unlock()
	assertPrivacyHTTPStatus(t, request.Clone(context.Background()), http.StatusServiceUnavailable)
	fixture.repository.mu.Lock()
	if len(fixture.repository.keys) != 1 || len(fixture.repository.receipts) != 0 {
		t.Error("failed transaction must preserve identity without completion")
	}
	fixture.repository.writeError = nil
	fixture.repository.mu.Unlock()
	// The failed operation consumed its nonce. A fresh proof may retry using
	// the same surviving identity; only a committed receipt permits replay.
	assertPrivacyHTTPStatus(t, request, http.StatusConflict)
	retry := fixture.signedRequest(t, http.MethodDelete, "/v1/privacy/data", 2)
	assertPrivacyHTTPStatus(t, retry.Clone(context.Background()), http.StatusNoContent)
	fixture.repository.mu.Lock()
	fixture.repository.lookupError = errors.New("synthetic lookup outage")
	fixture.repository.mu.Unlock()
	assertPrivacyHTTPStatus(t, retry.Clone(context.Background()), http.StatusServiceUnavailable)
	fixture.repository.mu.Lock()
	fixture.repository.lookupError = nil
	fixture.repository.mu.Unlock()
	assertPrivacyHTTPStatus(t, retry, http.StatusNoContent)
}

func assertPrivacyHTTPStatus(t *testing.T, request *http.Request, expected int) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != expected {
		t.Fatalf("expected HTTP %d, got %d %s", expected, response.StatusCode, body)
	}
}
