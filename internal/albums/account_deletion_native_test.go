package albums

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit local integration bridge, compiled only into tests. Synthetic
// credentials stay in a private file; the production gateway remains closed.
func TestMySQLAccountDeletionNativeBridge(t *testing.T) {
	dir := os.Getenv("ALBUMS_NATIVE_DELETION_DIR")
	if dir == "" {
		t.Skip("requires the native two-process integration runner")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("private bridge directory required")
	}
	manifest := filepath.Join(dir, "fixture.json")
	if _, err := os.Lstat(manifest); !os.IsNotExist(err) {
		t.Fatal("bridge directory was already used")
	}
	f := newPushFixture(t)
	accepted, _ := f.login(t, "native-accepted", f.now)
	withdrawn, _ := f.login(t, "native-withdrawn", f.now)
	other, _ := f.login(t, "native-withdrawn", f.now)
	router := deletionRouter(t, f, &loginHTTPLimiter{}, true)
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The bridge changes routing only. Authentication, request decoding,
		// service transactions and response serialization use the real router.
		mu.Lock()
		defer mu.Unlock()
		r.Host = "albums.example"
		key := r.Header.Get("Authorization") + " " + r.URL.Path
		counts[key]++
		if counts[key] == 1 && r.URL.Path == deletionPath && r.Header.Get("Authorization") == "Bearer "+withdrawn.Token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code":"injected_pre_commit_failure"}`))
			return
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		if counts[key] == 1 && ((r.URL.Path == deletionPath && r.Header.Get("Authorization") == "Bearer "+accepted.Token && response.Code == 202) ||
			(r.URL.Path == deletionPath+"/withdrawal" && r.Header.Get("Authorization") == "Bearer "+withdrawn.Token && response.Code == 200)) {
			// Fail after the transaction commits: native must retain its exact
			// intent and credential, then resolve it after process restart.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code":"injected_post_commit_failure"}`))
			return
		}
		for k, values := range response.Header() {
			w.Header()[k] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	defer server.Close()
	data, err := json.Marshal(map[string]any{"loopback": server.URL, "service": "albums.native.integration." + uuid.NewString(), "accepted": accepted, "withdrawn": withdrawn})
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
			t.Fatal("native test did not finish")
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
				goto verify
			}
		}
	}
verify:
	mu.Lock()
	defer mu.Unlock()
	if counts["Bearer "+accepted.Token+" "+deletionPath] != 2 || counts["Bearer "+accepted.Token+" "+deletionPath+"/withdrawal"] != 1 ||
		counts["Bearer "+withdrawn.Token+" "+deletionPath+"/withdrawal"] != 2 || counts["Bearer "+withdrawn.Token+" "+deletionPath] != 2 {
		t.Fatal("unexpected native request sequence")
	}
	var ledgerCount, acceptedCount, withdrawnCount int
	if err := f.r.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(account_id=?),0),COALESCE(SUM(account_id=?),0) FROM album_deletion_ledger`, accepted.AccountID, withdrawn.AccountID).Scan(&ledgerCount, &acceptedCount, &withdrawnCount); err != nil || ledgerCount != 1 || acceptedCount != 1 || withdrawnCount != 0 {
		t.Fatal("incorrect durable deletion outcomes", err)
	}
	if deletionAuthenticated(f, accepted.Token) || deletionAuthenticated(f, withdrawn.Token) || !deletionAuthenticated(f, other.Token) {
		t.Fatal("incorrect session scope after native roundtrip")
	}
	t.Log("native two-process roundtrip: one acceptance, one withdrawal, late acceptance rejected, other device retained")
}
