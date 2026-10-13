//go:build albums_live

package albums

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Explicit paid probes, one synthetic media item each. Real COS multipart/sealing/CI,
// isolated database and synthetic login/subscription. Never deletes any media.
func TestLiveCOSNativePhotoPipeline(t *testing.T)     { liveNativePipeline(t, Photo) }
func TestLiveCOSNativeVideoPipeline(t *testing.T)     { liveNativePipeline(t, Video) }
func TestLiveCOSNativeLivePhotoPipeline(t *testing.T) { liveNativePipeline(t, LivePhoto) }

func liveNativePipeline(t *testing.T, kind AssetKind) {
	t.Helper()
	prefix := "cloud-" + string(kind)
	expectedPhotos, expectedVideos, expectedPairs, expectedResources := 2, 0, 0, 1
	if kind == Video {
		expectedPhotos, expectedVideos = 0, 1
	}
	if kind == LivePhoto {
		expectedVideos, expectedPairs, expectedResources = 1, 1, 2
	}
	objects, root, ctx := liveRoundtripFixture(t)
	marker, err := os.OpenFile(filepath.Join(root, prefix+"-intent.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fresh cloud probe directory required; do not repeat unknown writes")
	}
	marker.Close()
	f := newPushFixture(t)
	session, _ := f.login(t, "native-cloud-"+string(kind), f.now)
	other, _ := f.login(t, "native-cloud-other", f.now)
	r := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox", time.Now)
	_, err = r.ApplySubscription(ctx, session.AccountID, 0, VerifiedAlbumSubscription{
		OriginalTransactionID: "synthetic-native-chain", TransactionID: "synthetic-native-transaction",
		ProductID: Cloud50GBAnnualProduct, Environment: "Sandbox", ExpiresAt: f.now.Add(time.Hour), SignedAt: f.now, Active: true}, f.now)
	if err != nil {
		t.Fatal("isolated entitlement setup failed")
	}
	roundtripJSON(t, root, prefix+"-intent.json", map[string]any{"ownerID": session.AccountID, "scope": "new synthetic " + string(kind) + "; no deletion; local database only"})
	service, err := NewUploadService(r, objects, UploadPolicy{MaxOriginalBytes: 32 << 20, MaxDerivedBytes: 16 << 20, UploadTTL: time.Hour, VerificationLease: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	multipart, err := NewMultipartService(r, objects, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &loginPipelineFixture{}
	challenges, _ := NewLoginChallengeService(&challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}, time.Now)
	login, _ := NewLoginService(challenges, pipeline, pipeline, pipeline)
	router, err := NewHTTPRouterWithLogin("albums.example", f.sessions, service, LoginHTTPOptions{
		Service: login, Sessions: f.sessions, Limiter: &loginHTTPLimiter{}, Multipart: multipart})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		request.Host = "albums.example"
		if request.URL.Path == "/v1/albums/library/versions" {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// Synthetic metadata only; never record credentials or read grants.
			_ = os.WriteFile(filepath.Join(root, "library-response.json"), response.Body.Bytes(), 0600)
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		router.ServeHTTP(w, request)
	}))
	defer server.Close()
	roundtripJSON(t, root, "fixture.json.tmp", map[string]any{"loopback": server.URL, "session": session, "other": other, "kind": kind})
	if err := os.Rename(filepath.Join(root, "fixture.json.tmp"), filepath.Join(root, "fixture.json")); err != nil {
		t.Fatal(err)
	}
	verify, _ := NewVerificationWorker(r, service, 1, time.Second)
	photos, _ := NewPhotoWorker(r, service, 2)
	videos, _ := NewVideoWorker(r, service, "p543daa336f7a4d4b896654b933f8881b", 1)
	pairs, _ := NewLivePairWorker(r, service, 1)
	finalize, _ := NewDerivedFinalizationWorker(r, 1, time.Now)
	counts := map[string]int{"originalsVerified": 0, "previewsStored": 0, "videosStored": 0, "pairsVerified": 0}
	defer func() {
		roundtripJSON(t, root, prefix+"-worker.json", map[string]any{"passed": !t.Failed(), "counts": counts})
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("cloud media probe timed out; inspect existing intent, do not repeat uploads")
		case <-tick.C:
			v, err := verify.RunOnce(ctx)
			if err != nil || v.Failed != 0 {
				t.Fatal("original verification failed; existing cloud objects retained")
			}
			counts["originalsVerified"] += v.Verified
			p, err := photos.RunOnce(ctx)
			if err != nil || p.Failed != 0 {
				t.Fatal("photo processing failed; existing cloud objects retained")
			}
			counts["previewsStored"] += p.Stored
			if kind == Video || kind == LivePhoto {
				video, err := videos.RunOnce(ctx)
				if err != nil || video.Failed != 0 {
					t.Fatal("video processing failed; existing intent retained; do not repeat submission")
				}
				counts["videosStored"] += video.Stored
			}
			if kind == LivePhoto {
				pair, err := pairs.RunOnce(ctx)
				if err != nil || pair.Failed != 0 {
					t.Fatal("live pair verification failed; retained objects require inspection")
				}
				counts["pairsVerified"] += pair.Verified
			}
			roundtripJSON(t, root, prefix+"-progress.json", counts)
			closed, err := finalize.RunOnce(ctx)
			if err != nil || closed.Failed != 0 || closed.Held != 0 {
				t.Fatal("derived quota finalization failed")
			}
			if _, err := os.Stat(filepath.Join(root, "done")); err == nil {
				goto verifyResult
			}
		}
	}
verifyResult:
	page, err := r.AssetVersions(context.Background(), session.AccountID, 0, 10)
	if err != nil || len(page.Versions) != 1 || counts["originalsVerified"] != 1 || counts["previewsStored"] != expectedPhotos || counts["videosStored"] != expectedVideos || counts["pairsVerified"] != expectedPairs {
		t.Fatal("unexpected published media/job counts")
	}
	version := page.Versions[0]
	if version.Manifest.Kind != kind {
		t.Fatal("unexpected media kind")
	}
	if len(version.Objects) != expectedResources {
		t.Fatal("unexpected source resource count")
	}
	for _, object := range version.Objects {
		reader, err := objects.OpenVersion(ctx, object.Key, object.VersionID)
		if err != nil {
			t.Fatal("sealed original unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(reader, object.SizeBytes+1))
		reader.Close()
		if err != nil || int64(len(data)) != object.SizeBytes || roundtripHash(data) != object.SHA256 {
			t.Fatal("archived original bytes changed")
		}
	}
	var used, reserved int64
	if err := f.r.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, session.AccountID).Scan(&used, &reserved); err != nil || used < version.OriginalBytes || reserved != 0 {
		t.Fatal("quota settlement incomplete", err)
	}
	roundtripJSON(t, root, prefix+"-backend-evidence.json", map[string]any{"passed": true, "versionID": version.VersionID, "original": version.Objects[0], "originals": version.Objects, "usedBytes": used, "reservedBytes": reserved, "counts": counts, "scope": "real COS/CI and native HTTP clients; isolated account, entitlement and MySQL"})
}
