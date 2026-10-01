package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/observability"
)

func TestAuthenticationLogsStageWithoutProofAndPreservesPublicErrors(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	nonces := attestation.NewMemoryNonceStore()
	keys := attestation.NewMemoryKeyStore()
	keys.Put(attestation.RegisteredKey{AppID: "health", KeyID: "private-key-sentinel"})
	nonce, err := nonces.Issue(context.Background(), "private-key-sentinel", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer()
	server.authenticator = attestation.NewService(nonces, keys,
		attestation.NewAppleAssertionVerifier("TEAM", "health.bundle"), func() time.Time { return now })
	var output bytes.Buffer
	server.httpMiddleware = observability.MiddlewareForApp(slog.New(slog.NewJSONHandler(&output, nil)), "health")
	router := server.newHTTPRouter()
	assertion := base64.StdEncoding.EncodeToString([]byte("private-invalid-assertion-sentinel"))
	for _, test := range []struct {
		status      int
		code, stage string
	}{
		{http.StatusUnauthorized, "authentication_failed", "assertion_cbor"},
		{http.StatusConflict, "replay_detected", "nonce"},
	} {
		output.Reset()
		request := httptest.NewRequest(http.MethodGet, "/v1/ai/quota", nil)
		request.Header.Set("X-Tellyouwhat-Request-ID", "19be2f9e-bd92-4699-b561-e3816092114c")
		request.Header.Set("X-Tellyouwhat-Key-ID", "private-key-sentinel")
		request.Header.Set("X-Tellyouwhat-Assertion", assertion)
		request.Header.Set("X-Tellyouwhat-Nonce", nonce)
		request.Header.Set("X-Tellyouwhat-Timestamp", now.Format(time.RFC3339))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
			t.Fatalf("public error changed: status=%d body=%s", response.Code, response.Body.String())
		}
		if !strings.Contains(output.String(), `"authentication_failure_stage":"`+test.stage+`"`) {
			t.Fatalf("diagnostic stage missing: %s", output.String())
		}
		for _, secret := range []string{"private-key-sentinel", assertion, nonce, "private-invalid-assertion-sentinel"} {
			if strings.Contains(output.String(), secret) || strings.Contains(response.Body.String(), secret) {
				t.Fatal("request proof leaked into logs or response")
			}
		}
		if strings.Contains(response.Body.String(), "authentication_failure_stage") {
			t.Fatal("internal diagnostic stage exposed to client")
		}
	}
}
