package albums

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/net/http2"
)

func apnsFixture(t *testing.T) (albumAPNSConfig, *ecdsa.PrivateKey, albumRetentionReminder, time.Time) {
	t.Helper()
	key := albumTestECKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	deadline := now.AddDate(0, 0, 30)
	due, expires, _ := reminderWindow(deadline, 30)
	c := albumRetentionReminder{ID: uuid.NewString(), Owner: uuid.NewString(), Token: uuid.NewString(), Environment: "Sandbox", Deadline: deadline, Due: due, Expires: expires, Until: now.Add(time.Minute), Days: 30, Attempts: 1, State: "pending"}
	return albumAPNSConfig{"TESTTEAM01", "TESTPUSH01", "production", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, key, c, now
}

func TestAlbumAPNSRealHTTP2RequestAndSignature(t *testing.T) {
	config, key, claim, now := apnsFixture(t)
	device := strings.Repeat("ab", 32)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.TLS == nil || r.Method != "POST" || r.Host != "api.push.apple.com" || r.URL.Path != "/3/device/"+device || r.URL.RawQuery != "" {
			t.Error("unexpected APNs request")
		}
		for name, expected := range map[string]string{"apns-id": claim.ID, "apns-topic": albumAppleAudience, "apns-push-type": "alert", "apns-priority": "5", "apns-collapse-id": "album-retention", "apns-expiration": strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10)} {
			if r.Header.Get(name) != expected {
				t.Error("incorrect header", name)
			}
		}
		claims := jwt.RegisteredClaims{}
		token, err := jwt.ParseWithClaims(strings.TrimPrefix(r.Header.Get("authorization"), "bearer "), &claims, func(t *jwt.Token) (any, error) {
			if t.Header["kid"] != config.KeyID {
				return nil, errors.New("incorrect key ID")
			}
			return &key.PublicKey, nil
		}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(config.TeamID), jwt.WithTimeFunc(func() time.Time { return now }))
		if err != nil || !token.Valid || claims.IssuedAt == nil || !claims.IssuedAt.Time.Equal(now) || claims.Subject != "" || len(claims.Audience) != 0 {
			t.Error("invalid APNs JWT", err)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if len(body) > 4096 || json.Unmarshal(body, &payload) != nil || len(payload) != 2 || payload["screen"] != "cloud-storage" {
			t.Error("invalid payload")
		}
		for _, private := range []string{device, claim.Owner, claim.Token, claim.ID, claim.Deadline.Format(time.RFC3339)} {
			if strings.Contains(string(body), private) {
				t.Error("private value in payload")
			}
		}
		w.Header().Set("apns-id", claim.ID)
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http2.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: server.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}}
	transport.DialTLSContext = func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		if addr != "api.push.apple.com:443" {
			t.Error("non-APNs destination")
		}
		dialer := tls.Dialer{Config: cfg}
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	s, err := newAlbumAPNS(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	result, err := s.sendRetention(context.Background(), device, claim)
	if err != nil || result.Status != "accepted" {
		t.Fatal("HTTP/2 send", result, err)
	}
}

func TestAlbumAPNSTokenCacheConcurrentRotationAndClockRollback(t *testing.T) {
	c, _, _, now := apnsFixture(t)
	s, err := newAlbumAPNS(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.providerToken(now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := s.providerToken(now.Add(49 * time.Minute))
			if err != nil || next != first {
				t.Error("token unnecessarily refreshed", err)
			}
		}()
	}
	wg.Wait()
	next, err := s.providerToken(now.Add(50 * time.Minute))
	if err != nil || next == first {
		t.Fatal("token not refreshed", err)
	}
	if _, err := s.providerToken(now); err == nil {
		t.Fatal("clock rollback accepted")
	}
	for _, v := range []any{c, s} {
		for _, text := range []string{fmt.Sprint(v), fmt.Sprintf("%#v", v)} {
			if strings.Contains(text, first) || strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, c.TeamID) {
				t.Fatal("secret logged")
			}
		}
	}
}

func TestAlbumAPNSResponseDisposition(t *testing.T) {
	config, _, claim, now := apnsFixture(t)
	for _, tc := range []struct {
		name, body, status, retry string
		code                      int
		invalid                   bool
	}{
		{"accepted", "", "accepted", "", 200, false},
		{"unregistered", fmt.Sprintf(`{"reason":"Unregistered","timestamp":%d}`, now.Add(-time.Hour).UnixMilli()), "unregistered", "", 410, false},
		{"expired-token", fmt.Sprintf(`{"reason":"ExpiredToken","timestamp":%d}`, now.Add(-time.Hour).UnixMilli()), "unregistered", "", 410, false},
		{"no-invalidation-time", `{"reason":"Unregistered"}`, "", "", 410, true},
		{"future-invalidation", fmt.Sprintf(`{"reason":"Unregistered","timestamp":%d}`, now.Add(time.Hour).UnixMilli()), "", "", 410, true},
		{"bad-device", `{"reason":"BadDeviceToken"}`, "rejected", "", 400, false},
		{"wrong-topic", `{"reason":"DeviceTokenNotForTopic"}`, "rejected", "", 400, false},
		{"expired-provider", `{"reason":"ExpiredProviderToken"}`, "rejected", "", 403, false},
		{"provider-rotation", `{"reason":"TooManyProviderTokenUpdates"}`, "rejected", "", 429, false},
		{"throttled", `{"reason":"TooManyRequests"}`, "retry", "1200", 429, false},
		{"server-error", `{"reason":"InternalServerError"}`, "retry", "", 500, false},
		{"unavailable", `{"reason":"Shutdown"}`, "retry", now.Add(time.Hour).Format(http.TimeFormat), 503, false},
		{"malformed", `secret response`, "", "", 503, true},
		{"oversize", strings.Repeat("x", 4097), "", "", 503, true},
		{"body-on-success", `{}`, "", "", 200, true},
		{"redirect", `{}`, "", "", 307, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, err := newAlbumAPNS(config, appleTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				h := http.Header{"Apns-Id": {claim.ID}, "Retry-After": {tc.retry}, "Location": {"https://untrusted.example"}}
				return &http.Response{StatusCode: tc.code, ProtoMajor: 2, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return now }
			result, err := s.sendRetention(context.Background(), strings.Repeat("cd", 32), claim)
			if (err != nil) != tc.invalid || result.Status != tc.status || calls != 1 {
				t.Fatal("response classification", result, err, calls)
			}
			if result.Status == "retry" && result.RetryAt.Before(now.Add(15*time.Minute)) {
				t.Fatal("retry too early")
			}
			if tc.name == "throttled" && !result.RetryAt.Equal(now.Add(20*time.Minute)) || tc.name == "unavailable" && !result.RetryAt.Equal(now.Add(time.Hour)) {
				t.Fatal("retry-after ignored")
			}
			if result.Status == "unregistered" && !result.InvalidatedAt.Equal(now.Add(-time.Hour)) {
				t.Fatal("invalidation time lost")
			}
		})
	}
}

func TestAlbumAPNSValidationAndUnknownOutcomes(t *testing.T) {
	config, _, claim, now := apnsFixture(t)
	for _, environment := range []string{"Sandbox", "Production", "", "https://untrusted.example"} {
		invalid := config
		invalid.Environment = environment
		if _, err := newAlbumAPNS(invalid, nil); err == nil {
			t.Fatal("implicit push environment accepted")
		}
	}
	config.Environment = "development"
	calls := 0
	s, _ := newAlbumAPNS(config, appleTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.sandbox.push.apple.com" {
			t.Error("incorrect development endpoint")
		}
		return nil, errors.New("private-device-token-and-provider-secret")
	}))
	s.now = func() time.Time { return now }
	for _, device := range []string{"", "abc", "ABCDEF", "../escape", strings.Repeat("ab", 513)} {
		if _, err := s.sendRetention(context.Background(), device, claim); err == nil {
			t.Fatal("invalid device token accepted")
		}
	}
	stale := claim
	stale.Until = now
	if _, err := s.sendRetention(context.Background(), "ab", stale); err == nil || calls != 0 {
		t.Fatal("invalid input contacted provider")
	}
	result, err := s.sendRetention(context.Background(), "ab", claim)
	if err != errAlbumAPNS || result.Status != "" || calls != 1 || strings.Contains(err.Error(), "private") {
		t.Fatal("unknown result retried, accepted or leaked details")
	}
	for _, proto := range []int{1, 2} {
		s, _ := newAlbumAPNS(config, appleTestTransport(func(r *http.Request) (*http.Response, error) {
			id := claim.ID
			if proto == 2 {
				id = uuid.NewString()
			}
			return &http.Response{StatusCode: 200, ProtoMajor: proto, Header: http.Header{"Apns-Id": {id}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}))
		s.now = func() time.Time { return now }
		if result, err := s.sendRetention(context.Background(), "ab", claim); err == nil || result.Status != "" {
			t.Fatal("unbound acknowledgement accepted")
		}
	}
}

func TestAlbumAPNSStageWindowAndRequestCancellation(t *testing.T) {
	config, _, claim, now := apnsFixture(t)
	calls := 0
	s, _ := newAlbumAPNS(config, appleTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("apns-expiration") != strconv.FormatInt(claim.Expires.Unix(), 10) {
			t.Error("APNs storage exceeded stage cutoff")
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatal("request was not limited to the remaining lease")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	s.now = func() time.Time { return now }
	for _, mutate := range []func(*albumRetentionReminder){
		func(c *albumRetentionReminder) { c.Token = "" },
		func(c *albumRetentionReminder) { c.State = "cancelled" },
		func(c *albumRetentionReminder) { c.Attempts = 0 },
		func(c *albumRetentionReminder) { c.Expires = c.Expires.Add(time.Second) },
		func(c *albumRetentionReminder) { c.Due = c.Due.Add(-time.Second) },
		func(c *albumRetentionReminder) { c.Environment = "other-app" },
	} {
		invalid := claim
		mutate(&invalid)
		if _, err := s.sendRetention(context.Background(), "ab", invalid); err == nil {
			t.Fatal("invalid lease accepted")
		}
	}
	for _, stamp := range []time.Time{now.Add(-time.Microsecond), claim.Expires} {
		s.now = func() time.Time { return stamp }
		if _, err := s.sendRetention(context.Background(), "ab", claim); err == nil {
			t.Fatal("outside window accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid claim contacted APNs")
	}
	s.now = func() time.Time { return now }
	claim.Days = 1
	claim.Deadline = now.Add(30 * time.Second)
	claim.Due, claim.Expires, _ = reminderWindow(claim.Deadline, 1)
	claim.Until = now.Add(20 * time.Millisecond)
	result, err := s.sendRetention(context.Background(), "ab", claim)
	if err != errAlbumAPNS || result.Status != "" || calls != 1 {
		t.Fatal("cancelled request accepted or retried")
	}
}
