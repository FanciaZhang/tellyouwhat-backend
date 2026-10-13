//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

// Only the synthetic fixture's initial upload is varied; production writes keep
// their standard class and a bucket lifecycle rule transitions old originals.
func liveOriginalPutOptions(t *testing.T) *cos.ObjectPutOptions {
	t.Helper()
	switch os.Getenv("ALBUM_COS_PROBE_STORAGE_CLASS") {
	case "":
		return nil
	case "MAZ_STANDARD_IA":
		return &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{XCosStorageClass: "MAZ_STANDARD_IA"}}
	default:
		t.Fatal("unsupported live probe storage class")
		return nil
	}
}

func assertLiveStorageClass(t *testing.T, ctx context.Context, s *COSObjects, object SealedObject, want string) {
	t.Helper()
	r, err := s.client.Object.Head(ctx, object.Key, nil, url.QueryEscape(object.VersionID))
	if r != nil && r.Body != nil {
		defer r.Body.Close()
	}
	if err != nil || r == nil || r.StatusCode != http.StatusOK ||
		r.Header.Get("x-cos-version-id") != object.VersionID || r.Header.Get("x-cos-storage-class") != want {
		t.Fatal("fixed-version storage class mismatch")
	}
}

func verifyLiveLowFrequencyOriginal(t *testing.T, ctx context.Context, s *COSObjects, object SealedObject, payload []byte) {
	t.Helper()
	if os.Getenv("ALBUM_COS_PROBE_STORAGE_CLASS") != "MAZ_STANDARD_IA" {
		return
	}
	assertLiveStorageClass(t, ctx, s, object, "MAZ_STANDARD_IA")
	body, err := s.OpenVersion(ctx, object.Key, object.VersionID)
	if err != nil {
		t.Fatal("production fixed-version low-frequency read failed")
	}
	actual, err := io.ReadAll(io.LimitReader(body, int64(len(payload)+1)))
	body.Close()
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("low-frequency original bytes mismatch")
	}
	signed, err := s.AuthorizeOriginalRead(ctx, object, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal("low-frequency original read grant failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		t.Fatal("read request invalid")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Do(req)
	if err != nil || r == nil {
		t.Fatal("signed low-frequency download failed")
	}
	actual, err = io.ReadAll(io.LimitReader(r.Body, int64(len(payload)+1)))
	r.Body.Close()
	if err != nil || r.StatusCode != http.StatusOK || r.Header.Get("x-cos-version-id") != object.VersionID || !bytes.Equal(actual, payload) {
		t.Fatal("signed low-frequency download mismatch")
	}
}
