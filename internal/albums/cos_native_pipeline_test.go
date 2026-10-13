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

// Explicit paid probe, one synthetic photo only. Real COS multipart/sealing/CI,
// isolated database and synthetic login/subscription. Never deletes any media.
func TestLiveCOSNativePhotoPipeline(t *testing.T) {
	objects, root, ctx := liveRoundtripFixture(t)
	marker, err := os.OpenFile(filepath.Join(root, "cloud-photo-intent.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("fresh cloud probe directory required; do not repeat unknown writes")
	}
	marker.Close()
	f := newPushFixture(t)
	session, _ := f.login(t, "native-cloud-photo", f.now)
	other, _ := f.login(t, "native-cloud-other", f.now)
	r := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox", time.Now)
	_, err = r.ApplySubscription(ctx, session.AccountID, 0, VerifiedAlbumSubscription{
		OriginalTransactionID: "synthetic-native-chain", TransactionID: "synthetic-native-transaction",
		ProductID: Cloud50GBAnnualProduct, Environment: "Sandbox", ExpiresAt: f.now.Add(time.Hour), SignedAt: f.now, Active: true}, f.now)
	if err != nil {
		t.Fatal("isolated entitlement setup failed")
	}
	roundtripJSON(t, root, "cloud-photo-intent.json", map[string]any{"ownerID": session.AccountID, "scope": "new synthetic photo; no deletion; local database only"})
	service, err := NewUploadService(r, objects, UploadPolicy{MaxOriginalBytes: 8 << 20, MaxDerivedBytes: 16 << 20, UploadTTL: time.Hour, VerificationLease: time.Minute}, time.Now)
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
	roundtripJSON(t, root, "fixture.json.tmp", map[string]any{"loopback": server.URL, "session": session, "other": other})
	if err := os.Rename(filepath.Join(root, "fixture.json.tmp"), filepath.Join(root, "fixture.json")); err != nil {
		t.Fatal(err)
	}
	verify, _ := NewVerificationWorker(r, service, 1, time.Second)
	photos, _ := NewPhotoWorker(r, service, 2)
	finalize, _ := NewDerivedFinalizationWorker(r, 1, time.Now)
	counts := map[string]int{"originalsVerified": 0, "previewsStored": 0}
	defer func() {
		roundtripJSON(t, root, "cloud-photo-worker.json", map[string]any{"passed": !t.Failed(), "counts": counts})
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("cloud photo probe timed out; inspect existing intent, do not repeat uploads")
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
			roundtripJSON(t, root, "cloud-photo-progress.json", counts)
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
	if err != nil || len(page.Versions) != 1 || counts["originalsVerified"] != 1 || counts["previewsStored"] != 2 {
		t.Fatal("unexpected published media/job counts")
	}
	version := page.Versions[0]
	if len(version.Objects) != 1 {
		t.Fatal("unexpected source resource count")
	}
	object := version.Objects[0]
	reader, err := objects.OpenVersion(ctx, object.Key, object.VersionID)
	if err != nil {
		t.Fatal("sealed original unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(reader, object.SizeBytes+1))
	reader.Close()
	if err != nil || int64(len(data)) != object.SizeBytes || roundtripHash(data) != object.SHA256 {
		t.Fatal("archived original bytes changed")
	}
	var used, reserved int64
	if err := f.r.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, session.AccountID).Scan(&used, &reserved); err != nil || used < object.SizeBytes || reserved != 0 {
		t.Fatal("quota settlement incomplete", err)
	}
	roundtripJSON(t, root, "cloud-photo-backend-evidence.json", map[string]any{"passed": true, "versionID": version.VersionID, "original": object, "usedBytes": used, "reservedBytes": reserved, "counts": counts, "scope": "real COS/CI and native HTTP clients; isolated account, entitlement and MySQL"})
}
