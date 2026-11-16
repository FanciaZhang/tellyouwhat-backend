package albums

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Only routing and a post-commit 503 are injected; authentication, JSON and SQL are real.
func TestMySQLAssetFavoritesNativeBridge(t *testing.T) {
	dir := os.Getenv("ALBUMS_NATIVE_DELETION_DIR")
	if dir == "" {
		t.Skip("requires the native favorites integration runner")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("private unused bridge directory required")
	}
	manifest := filepath.Join(dir, "fixture.json")
	if _, err := os.Lstat(manifest); !os.IsNotExist(err) {
		t.Fatal("bridge directory was already used")
	}
	base := newSharingFixture(t).version.Manifest
	base.Metadata = &AssetMetadata{PixelWidth: 100, PixelHeight: 100, Favorite: true}
	f := newSharingManifestFixture(t, base)
	auth, err := NewMySQLSessions(f.db, f.uploads.now)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewMySQLAccounts(albumDevelopmentAudience, f.db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := accounts.RegisterAppleLogin(context.Background(), AppleGrant{Identity: freshAppleIdentity(uuid.NewString()), RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	foreignSession, err := auth.Issue(context.Background(), foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.ApplySubscription(context.Background(), foreign.AccountID, 0, VerifiedAlbumSubscription{uuid.NewString(), uuid.NewString(), Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(1, 0, 0), *f.now, true}, *f.now); err != nil {
		t.Fatal(err)
	}
	before := cloudState(t, f)
	used, reserved := photoJournalBalance(t, f.db, f.login.AccountID)
	redisOptions, err := redis.ParseURL(os.Getenv("REDIS_TEST_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cache := redis.NewClient(redisOptions)
	defer cache.Close()
	if !t.Run("redis-operation-isolation", TestRedisAlbumLoginLimitAcrossInstances) {
		t.FailNow()
	}
	limiter, err := NewRedisLoginLimiter(cache, bytes.Repeat([]byte{53}, 32), 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router, err := newHTTPRouter("albums.example", auth, f.uploads, &LoginHTTPOptions{LibraryLimiter: limiter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var writes []AssetFavoriteInput
	var statuses []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		r.Host = "albums.example"
		isWrite := r.Method == http.MethodPut
		if isWrite {
			data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
			var input AssetFavoriteInput
			if err != nil || len(data) > 4096 || json.Unmarshal(data, &input) != nil {
				http.Error(w, "invalid native body", 400)
				return
			}
			writes = append(writes, input)
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		t.Logf("native favorite HTTP %s %s -> %d", r.Method, r.URL.Path, response.Code)
		if isWrite {
			statuses = append(statuses, response.Code)
			if len(writes) == 1 && response.Code == 200 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"code":"injected_post_commit_failure"}`))
				return
			}
		}
		for k, values := range response.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	defer server.Close()
	preflight, err := http.NewRequest(http.MethodGet, server.URL+"/v1/albums/library/changes?after=0&limit=50", nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Authorization", "Bearer "+f.session.Token)
	reply, err := server.Client().Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	reply.Body.Close()
	if reply.StatusCode != 200 {
		t.Fatal("catalogue fixture preflight failed", reply.StatusCode)
	}
	data, err := json.Marshal(map[string]any{"loopback": server.URL, "service": "albums.native.favorite." + uuid.NewString(), "session": f.session, "foreignSession": foreignSession})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifest+".tmp", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(manifest+".tmp", manifest); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(manifest)
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("native favorite tests did not finish")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
				goto verify
			}
		}
	}
verify:
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(statuses, []int{200, 200, 200, 200, 409, 200, 200, 409, 200, 404}) || len(writes) != 10 || writes[0] != writes[1] || writes[0] != writes[9] {
		t.Fatal("native exact replay, stale CAS or foreign-write boundary failed", statuses)
	}
	expected := []int64{0, 0, 1, 2, 1, 3, 4, 3, 5, 0}
	choices := []bool{true, true, false, true, false, false, true, false, false, true}
	for i, input := range writes {
		if input.ExpectedRevision != expected[i] || input.Favorite != choices[i] {
			t.Fatal("unexpected native revision or favorite", i)
		}
	}
	state, err := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now).AssetFavorite(context.Background(), f.login.AccountID, f.version.AssetID, *f.now)
	if err != nil || state.Revision != 6 || state.Favorite {
		t.Fatal("durable explicit false missing", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_asset_favorites`).Scan(&count); err != nil || count != 1 || f.signer.calls.Load() != 0 {
		t.Fatal("unexpected favorite rows or media authorization", err)
	}
	version, err := f.repo.AssetVersion(context.Background(), f.login.AccountID, f.version.VersionID)
	afterUsed, afterReserved := photoJournalBalance(t, f.db, f.login.AccountID)
	if err != nil || !reflect.DeepEqual(version, f.version) || !reflect.DeepEqual(before, cloudState(t, f)) || used != afterUsed || reserved != afterReserved {
		t.Fatal("favorite altered originals, lifecycle or quota", err)
	}
	evidence, err := json.Marshal(map[string]any{"handlerStatuses": statuses, "expectedRevisions": expected, "choices": choices, "exactReplay": true, "finalRevision": state.Revision, "finalFavorite": state.Favorite, "rows": count, "mediaAuthorizations": 0, "originalsLifecycleQuotaUnchanged": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "favorite-evidence.json"), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("native two-process favorite HTTP/SQL: exact replay, both conflict choices, explicit false recovery and account read/write isolation; no media authorization")
}
