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
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

// Loopback routing and one lost reply are test-only; sessions, JSON and SQL use real handlers.
func TestMySQLContentCorrectionsNativeBridge(t *testing.T) {
	dir := os.Getenv("ALBUMS_NATIVE_DELETION_DIR")
	if dir == "" {
		t.Skip("requires the native content-corrections integration runner")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("private unused bridge directory required")
	}
	manifest := filepath.Join(dir, "fixture.json")
	if _, err := os.Lstat(manifest); !os.IsNotExist(err) {
		t.Fatal("bridge directory was already used")
	}
	f := newSharingFixture(t)
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
	router, err := NewHTTPRouter("albums.example", auth, f.uploads)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var writes []ContentCorrectionInput
	var statuses []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		r.Host = "albums.example"
		isWrite := r.Method == http.MethodPut
		if isWrite {
			data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
			var input ContentCorrectionInput
			if err != nil || len(data) > 4096 || json.Unmarshal(data, &input) != nil {
				http.Error(w, "invalid native body", 400)
				return
			}
			writes = append(writes, input)
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
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
	data, err := json.Marshal(map[string]any{"loopback": server.URL, "service": "albums.native.content." + uuid.NewString(), "session": f.session, "foreignSession": foreignSession})
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
			t.Fatal("native content tests did not finish")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
				goto verify
			}
		}
	}
verify:
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(statuses, []int{200, 200, 200, 409, 200}) || len(writes) != 5 || !reflect.DeepEqual(writes[0], writes[1]) {
		t.Fatal("native writes did not replay one exact request then reject the stale edit", statuses)
	}
	if writes[0].ExpectedRevision != 0 || writes[2].ExpectedRevision != 1 || writes[3].ExpectedRevision != 1 || writes[4].ExpectedRevision != 2 ||
		!reflect.DeepEqual(writes[0].Included, []string{"dogs"}) || !reflect.DeepEqual(writes[2].Included, []string{"food"}) || !reflect.DeepEqual(writes[3].Included, []string{"cats"}) || len(writes[4].Included) != 0 {
		t.Fatal("unexpected native revision or choices")
	}
	state, err := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now).ContentCorrection(context.Background(), f.login.AccountID, f.version.VersionID, *f.now)
	if err != nil || state.Revision != 3 || len(state.Included) != 0 || len(state.Excluded) != 0 {
		t.Fatal("durable reset missing", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_content_corrections`).Scan(&count); err != nil || count != 1 || f.signer.calls.Load() != 0 {
		t.Fatal("unexpected correction rows or media authorization", err)
	}
	t.Log("native two-process HTTP/SQL: exact replay, stale CAS conflict, cloud choice, reset, fresh-index recovery and account isolation; no media grant")
}
