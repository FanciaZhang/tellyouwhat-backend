package albums

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type trackedReconcileBody struct {
	io.Reader
	closed *bool
}

func (b trackedReconcileBody) Close() error { *b.closed = true; return nil }

type failedReconcileReader struct{}

func (failedReconcileReader) Read([]byte) (int, error) { return 0, errors.New("connection lost") }

func TestCOSReconcilePinsVersionAndChecksFullBytes(t *testing.T) {
	data := "original-resource-bytes"
	hash := sha256.Sum256([]byte(data))
	resource := Resource{ID: "photo", SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	for _, scenario := range []string{"valid", "head-size", "missing-version", "version-race", "body-size", "wrong-bytes", "truncated", "extra-byte", "read-error", "head-error"} {
		t.Run(scenario, func(t *testing.T) {
			reads := 0
			closed := false
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/"+key {
					t.Fatal("wrong object")
				}
				if r.Method == http.MethodHead {
					if scenario == "head-error" {
						return nil, errors.New("connection lost")
					}
					response := cosResponse(r, "", "version-one")
					response.ContentLength = resource.SizeBytes
					if scenario == "head-size" {
						response.ContentLength++
					}
					if scenario == "missing-version" {
						response.Header.Del("x-cos-version-id")
					}
					return response, nil
				}
				if r.Method != http.MethodGet {
					t.Fatal("reconciliation mutated provider")
				}
				reads++
				if r.URL.Query().Get("versionId") != "version-one" {
					t.Fatal("read latest instead of observed version")
				}
				response := cosResponse(r, data, "version-one")
				var reader io.Reader = strings.NewReader(data)
				switch scenario {
				case "version-race":
					response.Header.Set("x-cos-version-id", "version-two")
				case "body-size":
					response.ContentLength++
				case "wrong-bytes":
					reader = strings.NewReader(strings.Repeat("x", len(data)))
				case "truncated":
					reader = strings.NewReader(data[:len(data)-1])
				case "extra-byte":
					reader = strings.NewReader(data + "x")
				case "read-error":
					reader = failedReconcileReader{}
				}
				response.Body = trackedReconcileBody{reader, &closed}
				return response, nil
			})
			version, err := s.VerifyStagingResource(context.Background(), key, resource)
			if scenario == "valid" {
				if err != nil || version != "version-one" {
					t.Fatal("valid original rejected", err)
				}
			} else if err == nil || version != "" {
				t.Fatal("unverified bytes accepted")
			}
			if reads > 0 && !closed {
				t.Fatal("provider response leaked")
			}
		})
	}
}
func TestCOSReconcileRejectsInvalidManifestBeforeNetwork(t *testing.T) {
	s := cosFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid input contacted COS"); return nil, nil })
	key := "albums/staging/" + uuid.NewString() + "/" + uuid.NewString() + "/photo"
	for _, r := range []Resource{{ID: "photo", SizeBytes: 1, SHA256: "bad"}, {ID: "other", SizeBytes: 1, SHA256: strings.Repeat("0", 64)}, {ID: "photo", SizeBytes: 0, SHA256: strings.Repeat("0", 64)}} {
		if _, err := s.VerifyStagingResource(context.Background(), key, r); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal("invalid manifest accepted", err)
		}
	}
}
