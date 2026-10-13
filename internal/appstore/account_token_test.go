package appstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSetAppAccountTokenUsesScopedSignedPutAndDoesNotFollowRedirect(t *testing.T) {
	now := time.Now().UTC()
	key := newTestECKey(t)
	target := "de0922d0-4ff0-5b4a-a17f-16ee381f277e"
	status := 200
	calls := 0
	responseBody := ""
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("billing mutation followed redirect") }))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.URL.EscapedPath() != "/inApps/v1/transactions/original%2Fid/appAccountToken" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("wrong mutation request")
		}
		assertTestBearerClaims(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "issuer-id", "cn.tellyouwhat.albums", now, &key.PublicKey)
		data, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if json.Unmarshal(data, &payload) != nil || len(payload) != 1 || payload["appAccountToken"] != target {
			t.Error("unexpected mutation body")
		}
		if status == 307 {
			w.Header().Set("Location", sink.URL)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()
	c := NewAPIClient(APIClientConfig{BaseURL: server.URL, KeyID: "key-id", IssuerID: "issuer-id", BundleID: "cn.tellyouwhat.albums", SigningKey: key, HTTPClient: server.Client(), Now: func() time.Time { return now }})
	for _, code := range []int{200, 204, 400, 401, 404, 429, 500, 307} {
		status = code
		err := c.SetAppAccountToken(context.Background(), "original/id", target)
		if (err == nil) != (code == 200) {
			t.Fatal(code, err)
		}
	}
	status = 200
	responseBody = "private-provider-body"
	if err := c.SetAppAccountToken(context.Background(), "original/id", target); !errors.Is(err, ErrInvalidAPIResponse) || strings.Contains(err.Error(), responseBody) {
		t.Fatal("untrusted response accepted/leaked", err)
	}
	before := calls
	for _, bad := range []string{"", "bad-token", "00000000-0000-0000-0000-000000000000"} {
		if err := c.SetAppAccountToken(context.Background(), "original/id", bad); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SetAppAccountToken(ctx, "original/id", target); err == nil {
		t.Fatal("cancelled request accepted")
	}
	if calls != before {
		t.Fatal("invalid/cancelled inputs sent")
	}
}
