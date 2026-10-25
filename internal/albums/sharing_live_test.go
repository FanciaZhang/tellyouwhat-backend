//go:build albums_live

package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Only the isolated fixture is seeded directly; signing and download use real COS.
// The private intent survives failed/uncertain writes for exact-version recovery.
func TestLiveCOSSharingOriginalDownloadAndRevocation(t *testing.T) {
	objects, root, ctx := liveRoundtripFixture(t)
	cleaner := liveSharingCleaner(t)
	var source bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			picture.SetRGBA(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 128, 255})
		}
	}
	if jpeg.Encode(&source, picture, &jpeg.Options{Quality: 90}) != nil {
		t.Fatal("synthetic JPEG generation failed")
	}
	f := newSharingContentFixture(t, source.Bytes())
	object := f.version.Objects[0]
	object.VersionID = ""
	intent, err := os.OpenFile(filepath.Join(root, "sharing-intent.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("fresh private probe directory required; do not repeat an uncertain upload")
	}
	err = json.NewEncoder(intent).Encode(object)
	closeErr := intent.Close()
	if err != nil || closeErr != nil {
		t.Fatal("intent write failed")
	}
	evidence := map[string]any{"scope": "isolated MySQL sharing service and real COS; synthetic JPEG; no Apple or deployed gateway", "bytes": source.Len(), "sha256": object.SHA256}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if concreteCOSVersion(object.VersionID) {
			err := cleaner.deleteObjectVersion(cleanup, object.Key, object.VersionID)
			evidence["exactVersionCleanup"] = err == nil
			if err != nil {
				t.Error("exact synthetic version cleanup failed; retain private sharing-object.json for recovery")
			}
		} else {
			evidence["exactVersionCleanup"] = false
			t.Error("upload outcome unknown; retain private intent for recovery")
		}
		evidence["passed"] = !t.Failed()
		roundtripJSON(t, root, "sharing-evidence.json", evidence)
	})
	response, err := objects.client.Object.Put(ctx, object.Key, bytes.NewReader(source.Bytes()), nil)
	if response != nil {
		object.VersionID = response.Header.Get("x-cos-version-id")
		if response.Body != nil {
			response.Body.Close()
		}
	}
	roundtripJSON(t, root, "sharing-object.json", object)
	if err != nil || response == nil || response.StatusCode != 200 || !concreteCOSVersion(object.VersionID) {
		t.Fatal("synthetic upload failed or uncertain; no retry")
	}
	sealed, _ := json.Marshal([]SealedObject{object})
	if _, err = f.db.ExecContext(ctx, `UPDATE album_asset_versions SET sealed_objects=? WHERE owner_id=? AND version_id=?`, sealed, f.login.AccountID, f.version.VersionID); err != nil {
		t.Fatal("isolated fixture update failed")
	}
	f.uploads, err = NewUploadService(f.repo, objects, UploadPolicy{1 << 30, 1 << 20, 4 * time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal("upload service configuration failed")
	}
	f.service, err = NewShareService(f.repo, f.uploads, "https://shares.example", [][]byte{f.key}, time.Now)
	if err != nil {
		t.Fatal("share service configuration failed")
	}
	link, token := f.create(t, "合成123", 1)
	requestID := uuid.NewString()
	if _, err = f.service.Claim(ctx, token, "wrong", requestID); !errors.Is(err, ErrSharePassword) {
		t.Fatal("wrong password accepted")
	}
	claim, err := f.service.Claim(ctx, token, "合成123", requestID)
	if err != nil {
		t.Fatal("claim failed")
	}
	replay, err := f.service.Claim(ctx, token, "合成123", requestID)
	if err != nil || replay != claim {
		t.Fatal("claim retry was not idempotent")
	}
	if _, err = f.service.Claim(ctx, token, "合成123", uuid.NewString()); !errors.Is(err, ErrShareLimit) {
		t.Fatal("claim limit not enforced")
	}
	grants, err := f.service.ReadGrants(ctx, claim.Token)
	if err != nil || len(grants) != 1 {
		t.Fatal("read grant failed")
	}
	grant := grants[0]
	parsed, err := url.Parse(grant.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Query().Get("versionId") != object.VersionID || grant.ObjectVersionID != object.VersionID || grant.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatal("grant version or lifetime mismatch")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	download := func(partial bool) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, "GET", grant.URL, nil)
		if err != nil {
			t.Fatal("invalid grant request")
		}
		if partial {
			request.Header.Set("Range", "bytes=0-31")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("real signed download failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, int64(source.Len()+1)))
		expected, status := source.Bytes(), 200
		if partial {
			expected, status = expected[:32], 206
		}
		disposition, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		if err != nil || response.StatusCode != status || !bytes.Equal(data, expected) || parseErr != nil || disposition != "attachment" || params["filename"] != "回忆.jpeg" || response.Header.Get("x-cos-version-id") != object.VersionID {
			t.Fatalf("real download bytes, attachment, status or version mismatch (status %d)", response.StatusCode)
		}
	}
	download(false)
	download(true)
	evidence["fullBytesAndRangeVerified"] = true
	if err = f.service.Revoke(ctx, f.login.AccountID, link.ID); err != nil {
		t.Fatal("revoke failed")
	}
	if _, err = f.service.Lookup(ctx, token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("revoked lookup accepted")
	}
	if _, err = f.service.ReadGrants(ctx, claim.Token); !errors.Is(err, ErrShareNotAvailable) {
		t.Fatal("revoked grant renewal accepted")
	}
	// COS grants already issued remain usable until their short expiry.
	download(false)
	evidence["revokedLinkDeniedNewGrants"] = true
	evidence["existingGrantStillReadableAfterRevoke"] = true
}

func liveSharingCleaner(t *testing.T) *COSObjects {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	var keys [2]struct{ SecretId, SecretKey string }
	for i, name := range []string{"cleanup-credentials.json", "upload-cos.json"} {
		data, err := os.ReadFile(filepath.Join(home, ".config/tellyouwhat/albums-test", name))
		if err != nil || json.Unmarshal(data, &keys[i]) != nil {
			t.Fatal("dedicated cleanup credentials required before upload")
		}
	}
	cleaner, err := NewCOSObjects(COSConfig{Bucket: "tellyouwhat-albums-1251995834", Region: "ap-guangzhou", SecretID: keys[0].SecretId, SecretKey: keys[0].SecretKey, UploadSecretID: keys[1].SecretId, UploadSecretKey: keys[1].SecretKey})
	if err != nil {
		t.Fatal("cleanup configuration invalid")
	}
	return cleaner
}

// Retry only the recorded synthetic version; never delete by prefix or latest.
func TestLiveCOSSharingCleanupRecovery(t *testing.T) {
	objects, root, ctx := liveRoundtripFixture(t)
	var intent, object SealedObject
	roundtripRead(t, root, "sharing-intent.json", &intent)
	roundtripRead(t, root, "sharing-object.json", &object)
	if intent.VersionID != "" || !concreteCOSVersion(object.VersionID) || !albumObjectKey(object.Key, "originals") || object.ResourceID != "photo" || object.SizeBytes <= 0 || object.SizeBytes > 8<<10 {
		t.Fatal("invalid synthetic cleanup record")
	}
	intent.VersionID = object.VersionID
	if intent != object {
		t.Fatal("cleanup record does not match intent")
	}
	if liveSharingCleaner(t).deleteObjectVersion(ctx, object.Key, object.VersionID) != nil {
		t.Fatal("exact synthetic cleanup retry failed")
	}
	response, err := objects.client.Object.Get(ctx, object.Key, nil, url.QueryEscape(object.VersionID))
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatal("deleted synthetic version still readable or absence unconfirmed")
	}
	roundtripJSON(t, root, "sharing-cleanup-recovery.json", map[string]any{"exactVersionDeleted": true, "fixedVersionReadStatus": 404})
}
