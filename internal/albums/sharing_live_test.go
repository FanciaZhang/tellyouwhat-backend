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
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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
	manifest := Manifest{AssetID: uuid.NewString(), SourceRevision: "sharing-media-v1", Kind: Photo}
	content := map[string][]byte{"photo": source.Bytes()}
	manifest.Resources = []Resource{{ID: "photo", Role: OriginalPhoto, Descriptor: &ResourceDescriptor{PhotoKitType: 1, Filename: "回忆.jpeg", UniformTypeIdentifier: "public.jpeg"}}}
	switch os.Getenv("ALBUM_SHARE_MEDIA_KIND") {
	case "", "photo":
	case "video":
		manifest.Kind = Video
		content = map[string][]byte{"video": roundtripBytes(t, root, "source.mp4")}
		manifest.Resources = []Resource{{ID: "video", Role: OriginalVideo, Descriptor: &ResourceDescriptor{PhotoKitType: 2, Filename: "散步视频.mp4", UniformTypeIdentifier: "public.mpeg-4"}}}
	case "live_photo":
		manifest.Kind = LivePhoto
		content = map[string][]byte{"photo": roundtripBytes(t, root, "pair.jpg"), "video": roundtripBytes(t, root, "pair.mov")}
		manifest.Resources = append(manifest.Resources, Resource{ID: "video", Role: PairedVideo, Descriptor: &ResourceDescriptor{PhotoKitType: 9, Filename: "回忆.MOV", UniformTypeIdentifier: "com.apple.quicktime-movie"}})
		var timing LivePhotoTiming
		roundtripRead(t, root, "timing.json", &timing)
		manifest.LivePhotoTiming = &timing
		var pair struct {
			PhotoWidth  int `json:"photoWidth"`
			PhotoHeight int `json:"photoHeight"`
		}
		roundtripRead(t, root, "pair-inspection.json", &pair)
		manifest.Metadata = &AssetMetadata{PixelWidth: pair.PhotoWidth, PixelHeight: pair.PhotoHeight, MediaSubtypes: 8}
	default:
		t.Fatal("unsupported synthetic media kind")
	}
	for i := range manifest.Resources {
		r := &manifest.Resources[i]
		r.SizeBytes = int64(len(content[r.ID]))
		r.SHA256 = roundtripHash(content[r.ID])
	}
	f := newSharingManifestFixture(t, manifest)
	sealed := append([]SealedObject(nil), f.version.Objects...)
	for i := range sealed {
		sealed[i].VersionID = ""
	}
	intent, err := os.OpenFile(filepath.Join(root, "sharing-intent.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("fresh private probe directory required; no uncertain upload retry")
	}
	err = json.NewEncoder(intent).Encode(sealed)
	closeErr := intent.Close()
	if err != nil || closeErr != nil {
		t.Fatal("intent write failed")
	}
	evidence := map[string]any{"scope": "isolated MySQL and real COS; synthetic media; no Apple or deployed gateway", "kind": manifest.Kind, "resources": len(sealed)}
	attempted := 0
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cleaned := true
		for i, object := range sealed {
			if object.VersionID == "" {
				if i < attempted {
					cleaned = false
					t.Error("uncertain upload; retain private intent")
				}
				continue
			}
			if !concreteCOSVersion(object.VersionID) || cleaner.deleteObjectVersion(cleanup, object.Key, object.VersionID) != nil {
				cleaned = false
				t.Error("exact synthetic cleanup failed; retain recovery records")
			}
		}
		evidence["exactVersionCleanup"] = cleaned
		evidence["passed"] = !t.Failed()
		roundtripJSON(t, root, "sharing-evidence.json", evidence)
	})
	for i := range sealed {
		object := &sealed[i]
		attempted = i + 1
		response, putErr := objects.client.Object.Put(ctx, object.Key, bytes.NewReader(content[object.ResourceID]), nil)
		if response != nil {
			object.VersionID = response.Header.Get("x-cos-version-id")
			if response.Body != nil {
				response.Body.Close()
			}
		}
		roundtripJSON(t, root, "sharing-object.json", sealed)
		if putErr != nil || response == nil || response.StatusCode != 200 || !concreteCOSVersion(object.VersionID) {
			t.Fatal("synthetic upload failed or uncertain; no retry")
		}
	}
	encoded, _ := json.Marshal(sealed)
	if _, err = f.db.ExecContext(ctx, `UPDATE album_asset_versions SET sealed_objects=? WHERE owner_id=? AND version_id=?`, encoded, f.login.AccountID, f.version.VersionID); err != nil {
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
	if err != nil || len(grants) != len(sealed) {
		t.Fatal("read grant failed")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	download := func(partial bool) {
		t.Helper()
		for i, grant := range grants {
			object := sealed[i]
			resource := manifest.Resources[i]
			parsed, err := url.Parse(grant.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Query().Get("versionId") != object.VersionID || grant.ObjectVersionID != object.VersionID || grant.ResourceID != object.ResourceID || grant.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
				t.Fatal("grant version, resource or lifetime mismatch")
			}
			request, err := http.NewRequestWithContext(ctx, "GET", grant.URL, nil)
			if err != nil {
				t.Fatal("invalid grant")
			}
			if partial {
				request.Header.Set("Range", "bytes=0-31")
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("real signed download failed")
			}
			data, readErr := io.ReadAll(io.LimitReader(response.Body, object.SizeBytes+1))
			response.Body.Close()
			expected, status := content[object.ResourceID], 200
			if partial {
				expected, status = expected[:32], 206
			}
			disposition, params, parseErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
			if readErr != nil || response.StatusCode != status || !bytes.Equal(data, expected) || parseErr != nil || disposition != "attachment" || params["filename"] != resource.Descriptor.Filename || response.Header.Get("x-cos-version-id") != object.VersionID {
				t.Fatal("download bytes, filename, status or version mismatch")
			}
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
	if script := os.Getenv("ALBUM_SHARE_BROWSER_SCRIPT"); script != "" {
		runLiveSharingBrowser(t, ctx, f, root, script, manifest)
		evidence["recipientBrowserPassed"] = true
	}
}

func runLiveSharingBrowser(t *testing.T, ctx context.Context, f sharingFixture, root, script string, media Manifest) {
	t.Helper()
	sessions, err := NewMySQLSessions(f.db, time.Now)
	if err != nil {
		t.Fatal("browser sessions setup failed")
	}
	pipeline := &loginPipelineFixture{}
	challenges, _ := NewLoginChallengeService(&challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}, time.Now)
	login, _ := NewLoginService(challenges, pipeline, pipeline, pipeline)
	router, err := NewHTTPRouterWithLogin("albums.example", sessions, f.uploads, LoginHTTPOptions{
		Service: login, Sessions: sessions, Limiter: &loginHTTPLimiter{}, Shares: f.service,
		ShareLimiter: &loginHTTPLimiter{}, ShareClaimLimiter: &loginHTTPLimiter{}})
	if err != nil {
		t.Fatal("browser share router setup failed")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		request.Host = "albums.example"
		router.ServeHTTP(w, request)
	}))
	defer server.Close()
	manifest := filepath.Join(root, "browser-fixture.json")
	defer os.Remove(manifest)
	roundtripJSON(t, root, "browser-fixture.json", map[string]any{
		"apiOrigin": server.URL, "siteOrigin": "https://shares.example", "session": f.session.Token,
		"nativeSession": f.session,
		"versionID":     f.version.VersionID, "kind": media.Kind, "resources": media.Resources,
	})
	command := exec.CommandContext(ctx, "node", script, manifest)
	command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
	command.WaitDelay = 15 * time.Second
	// Browser diagnostics may contain bearer URLs; only retain them privately.
	log, err := os.OpenFile(filepath.Join(root, "browser-private.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("browser log setup failed")
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err = command.Run(); err != nil {
		t.Fatal("recipient browser probe failed; inspect private log")
	}
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
	var intent, objectsToDelete []SealedObject
	roundtripRead(t, root, "sharing-intent.json", &intent)
	roundtripRead(t, root, "sharing-object.json", &objectsToDelete)
	if len(intent) < 1 || len(intent) > 2 || len(intent) != len(objectsToDelete) {
		t.Fatal("invalid synthetic cleanup record count")
	}
	for i, object := range objectsToDelete {
		if intent[i].VersionID != "" || !concreteCOSVersion(object.VersionID) || !albumObjectKey(object.Key, "originals") || (object.ResourceID != "photo" && object.ResourceID != "video") || object.SizeBytes <= 0 || object.SizeBytes > 32<<20 {
			t.Fatal("invalid synthetic cleanup record")
		}
		intent[i].VersionID = object.VersionID
		if intent[i] != object {
			t.Fatal("cleanup record does not match intent")
		}
	}
	for _, object := range objectsToDelete {
		if liveSharingCleaner(t).deleteObjectVersion(ctx, object.Key, object.VersionID) != nil {
			t.Fatal("exact synthetic cleanup failed")
		}
		response, err := objects.client.Object.Get(ctx, object.Key, nil, url.QueryEscape(object.VersionID))
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
			t.Fatal("synthetic version absence unconfirmed")
		}
	}
	roundtripJSON(t, root, "sharing-cleanup-recovery.json", map[string]any{"exactVersionDeleted": true, "resources": len(objectsToDelete), "fixedVersionReadStatus": 404})
}
