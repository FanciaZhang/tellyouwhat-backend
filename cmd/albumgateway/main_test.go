package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tellyouwhat/backend/internal/albums"
)

func TestContainerProbeChecksHostRouteAndBoundedReadinessResponse(t *testing.T) {
	for _, mode := range []string{"ready", "other response", "oversize", "redirect", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "api.albums.example" || r.Method != "GET" || r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "" {
					t.Error("incorrect readiness request")
				}
				switch mode {
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(302)
				case "unavailable":
					w.WriteHeader(503)
					w.Write([]byte(`{"code":"not_ready"}`))
				case "other response":
					w.Write([]byte(`{"code":"not_ready"}`))
				case "oversize":
					w.Write([]byte(strings.Repeat("x", 1000)))
				default:
					w.Write([]byte(`{"code":"ok"}`))
				}
			}))
			defer server.Close()
			address := strings.TrimPrefix(server.URL, "http://")
			config := albums.GatewayConfig{ListenAddress: address, APIHost: "api.albums.example"}
			if err := checkReady(config); (err == nil) != (mode == "ready") {
				t.Fatal("wrong readiness result")
			}
			if mode == "ready" {
				config.ListenAddress = "0.0.0.0:" + strings.Split(address, ":")[1]
				if err := checkReady(config); err != nil {
					t.Fatal("container wildcard bind probe failed", err)
				}
			}
		})
	}
}
