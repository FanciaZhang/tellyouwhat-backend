package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/albums"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/config"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/gateway"
)

func newAlbumAttestedHandler(app config.AppConfig, nonces attestation.NonceStore, keys interface {
	attestation.KeyStore
	attestation.EnrollmentKeyStore
}, roots *x509.CertPool, readiness gateway.Readiness, next http.Handler, guardBootstrap func(http.Handler) http.Handler) http.Handler {
	identity := app.Registry
	enrollment := attestation.NewEnrollmentService(attestation.EnrollmentConfig{
		AppID: string(identity.ID), Environment: app.AttestationEnvironment, AllowedBuilds: app.AllowedBuilds,
	}, nonces, keys, attestation.NewAppleAttestationVerifier(identity.TeamID, identity.BundleID, app.AttestationEnvironment, roots), time.Now)
	auth := attestation.NewService(nonces, keys, attestation.NewAppleAssertionVerifier(identity.TeamID, identity.BundleID), time.Now).RequireEnvironment(app.AttestationEnvironment)
	bootstrap := guardBootstrap(gateway.NewAttestationRouter(enrollment))
	return &albumAttestedHandler{readiness: readiness, bootstrap: bootstrap, auth: auth, next: next}
}

type albumAttestedHandler struct {
	readiness gateway.Readiness
	bootstrap http.Handler
	auth      *attestation.Service
	next      http.Handler
}

func (h *albumAttestedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if h.readiness == nil || h.readiness.Ready(ctx) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"not_ready"}`)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/v1/attest/") {
		h.bootstrap.ServeHTTP(w, r)
		return
	}
	if !albums.RequestNeedsAppAttest(r) {
		h.next.ServeHTTP(w, r)
		return
	}
	fail := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"code":"`+code+`"}`)
	}
	// Do not accept ambiguous headers or any simulator/development authentication scheme.
	for _, name := range []string{"Request-ID", "Key-ID", "Assertion", "Nonce", "Timestamp"} {
		values := r.Header.Values("X-Tellyouwhat-" + name)
		if len(values) != 1 || values[0] == "" || len(values[0]) > 512<<10 {
			fail(401, "app_attestation_required")
			return
		}
	}
	requestID, err := uuid.Parse(r.Header.Get("X-Tellyouwhat-Request-ID"))
	if err != nil || requestID == uuid.Nil || len(r.Header.Values("X-Tellyouwhat-Authentication")) != 0 || len(r.Header.Values("X-Tellyouwhat-Dev-Activation")) != 0 {
		fail(401, "app_attestation_required")
		return
	}
	var body []byte
	if r.Body != nil {
		reader := http.MaxBytesReader(w, r.Body, 2<<20)
		body, err = io.ReadAll(reader)
		_ = reader.Close()
	}
	if err != nil {
		fail(413, "request_too_large")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	// Album queries select paginated resources and must be bound as sent.
	principal, err := h.auth.Authenticate(r.Context(), attestation.RequestProof{
		Method: r.Method, Path: r.URL.RequestURI(), RequestID: requestID.String(),
		KeyID: r.Header.Get("X-Tellyouwhat-Key-ID"), Assertion: r.Header.Get("X-Tellyouwhat-Assertion"),
		Nonce: r.Header.Get("X-Tellyouwhat-Nonce"), Timestamp: r.Header.Get("X-Tellyouwhat-Timestamp"), BodySHA256: contracts.BodySHA256(body),
	})
	if err != nil || principal.AppID != "albums" {
		switch {
		case errors.Is(err, attestation.ErrUnavailable):
			fail(503, "app_attestation_unavailable")
		case errors.Is(err, attestation.ErrReplay):
			fail(409, "app_attestation_replayed")
		case errors.Is(err, attestation.ErrAuthentication) && attestation.FailureStage(err) == "key_lookup":
			fail(401, "app_attestation_key_unknown")
		default:
			fail(401, "app_attestation_required")
		}
		return
	}
	h.next.ServeHTTP(w, r)
}
