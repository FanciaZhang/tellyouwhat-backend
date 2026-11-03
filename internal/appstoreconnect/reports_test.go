package appstoreconnect

import (
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfferReportsUseScopedDailyAPIAndRejectFailures(t *testing.T) {
	for _, status := range []int{200, 403, 404, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/salesReports" || r.Method != "GET" {
					t.Fatal("wrong report endpoint")
				}
				q := r.URL.Query()
				for k, v := range map[string]string{"frequency": "DAILY", "reportDate": "2026-09-08", "reportType": "SUBSCRIPTION_OFFER_CODE_REDEMPTION", "reportSubType": "SUMMARY", "version": "1_0", "vendorNumber": "12345678"} {
					if q.Get("filter["+k+"]") != v {
						t.Errorf("wrong report filter %s", k)
					}
				}
				claims := decodeClaims(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				scope := claims["scope"].([]any)
				if len(scope) != 1 || scope[0] != "GET /v1/salesReports" {
					t.Fatal("report read lacks scoped JWT")
				}
				w.WriteHeader(status)
				if status == 200 {
					gz := gzip.NewWriter(w)
					gz.Write([]byte("report fixture"))
					gz.Close()
				} else {
					w.Write([]byte("sensitive provider diagnostics"))
				}
			}))
			defer server.Close()
			client, err := NewClient(Config{BaseURL: server.URL, IssuerID: "issuer", KeyID: "key", SubscriptionID: "456", AppAppleID: "123", VendorNumber: "12345678", SigningKey: key})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := client.DownloadOfferReport(context.Background(), "2026-09-08")
			if status == 200 {
				if err != nil || string(raw) != "report fixture" {
					t.Fatal(err)
				}
				return
			}
			if len(raw) != 0 || err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("failure became data or leaked provider output")
			}
			if status == 404 && !errors.Is(err, ErrReportNotAvailable) {
				t.Fatal("missing report not distinguished")
			}
		})
	}
}
