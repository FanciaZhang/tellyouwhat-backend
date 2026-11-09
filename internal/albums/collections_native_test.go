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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Seeded identity only: native requests use real session checks, HTTP handlers and SQL.
func TestMySQLCollectionsNativeBridge(t *testing.T) {
	dir := os.Getenv("ALBUMS_NATIVE_DELETION_DIR")
	if dir == "" {
		t.Skip("requires the native collections integration runner")
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
	limiter, err := NewRedisLoginLimiter(cache, bytes.Repeat([]byte{53}, 32), 1000, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router, err := newHTTPRouter("albums.example", auth, f.uploads, &LoginHTTPOptions{LibraryLimiter: limiter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	type write struct {
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
		Status int             `json:"status"`
	}
	var writes []write
	lost := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		r.Host = "albums.example"
		var body []byte
		if r.Method == http.MethodPut {
			var readErr error
			body, readErr = io.ReadAll(io.LimitReader(r.Body, 4097))
			if readErr != nil || len(body) > 4096 || !json.Valid(body) {
				http.Error(w, "invalid native body", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		t.Logf("native collections HTTP %s %s -> %d", r.Method, r.URL.Path, response.Code)
		if r.Method == http.MethodPut {
			writes = append(writes, write{r.URL.Path, append(json.RawMessage(nil), body...), response.Code})
			if strings.Contains(r.URL.Path, "/members/") && response.Code == 200 && !lost {
				lost = true
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
	data, err := json.Marshal(map[string]any{"loopback": server.URL, "service": "albums.native.collection." + uuid.NewString(), "session": f.session, "foreignSession": foreignSession})
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
			t.Fatal("native collection tests did not finish")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
				goto verify
			}
		}
	}
verify:
	mu.Lock()
	defer mu.Unlock()
	var statuses []int
	for _, v := range writes {
		statuses = append(statuses, v.Status)
	}
	if !lost || !reflect.DeepEqual(statuses, []int{200, 200, 200, 200, 200, 409, 200, 409, 200, 200, 200, 404}) {
		t.Fatal("native writes, replay or CAS mismatch", statuses)
	}
	if writes[1].Path != writes[2].Path || !bytes.Equal(writes[1].Body, writes[2].Body) || !bytes.Equal(writes[1].Body, writes[11].Body) {
		t.Fatal("immutable member replay changed")
	}
	expected := []int64{0, 0, 0, 1, 2, 1, 1, 1, 2, 3, 3, 0}
	for i, v := range writes {
		var request struct {
			ExpectedRevision int64 `json:"expectedRevision"`
		}
		if json.Unmarshal(v.Body, &request) != nil || request.ExpectedRevision != expected[i] {
			t.Fatal("wrong native revision", i)
		}
	}
	id := "324aa069-b7da-4d23-88e7-4c8d54bd4749"
	parent, err := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now).Collection(context.Background(), f.login.AccountID, id, *f.now)
	if err != nil || parent.Revision != 4 || !parent.Deleted || parent.Name != "" {
		t.Fatal("durable deleted grouping missing", parent, err)
	}
	var revision int64
	var present bool
	var parents, members int
	if err = f.db.QueryRow(`SELECT revision,present FROM album_collection_members WHERE owner_id=? AND collection_id=? AND asset_id=?`, f.login.AccountID, id, f.version.AssetID).Scan(&revision, &present); err != nil || revision != 4 || present {
		t.Fatal("explicit member removal missing", err)
	}
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM album_collections`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM album_collection_members`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if parents != 1 || members != 1 || f.signer.calls.Load() != 0 {
		t.Fatal("extra metadata rows or media grant", parents, members)
	}
	version, err := f.repo.AssetVersion(context.Background(), f.login.AccountID, f.version.VersionID)
	afterUsed, afterReserved := photoJournalBalance(t, f.db, f.login.AccountID)
	if err != nil || !reflect.DeepEqual(version, f.version) || !reflect.DeepEqual(before, cloudState(t, f)) || used != afterUsed || reserved != afterReserved {
		t.Fatal("grouping altered original, lifecycle or quota", err)
	}
	evidence, err := json.Marshal(map[string]any{"writes": writes, "exactMemberReplay": true, "parentRevision": parent.Revision, "parentDeleted": parent.Deleted, "memberRevision": revision, "memberPresent": present, "parentRows": parents, "memberRows": members, "mediaAuthorizations": 0, "originalsLifecycleQuotaUnchanged": true, "identity": "seeded synthetic sessions; no Apple or COS requests"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "collection-evidence.json"), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("native three-process HTTP/SQL: parent-first publish, lost member receipt replay, independent indexes, both conflict choices, explicit removal, deleted-group recovery and account isolation")
}
