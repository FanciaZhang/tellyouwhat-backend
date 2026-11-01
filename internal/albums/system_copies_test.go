package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func TestSystemCopyKindsAndIdentifierBoundary(t *testing.T) {
	for _, media := range []AssetKind{Photo, Video, LivePhoto, "unknown"} {
		for _, kind := range []string{"restored_original", "daily_jpeg_v1", "daily_png_v1", "daily_mp4_1080_v1", "daily_live_pair_v1", "grid_jpeg_v1", "unknown"} {
			expected := media != "unknown" && (kind == "restored_original" || media == Photo && (kind == "daily_jpeg_v1" || kind == "daily_png_v1") || media == Video && kind == "daily_mp4_1080_v1" || media == LivePhoto && kind == "daily_live_pair_v1")
			if validSystemCopyKind(kind, media) != expected {
				t.Fatal(media, kind)
			}
		}
	}
	for _, value := range []string{"", strings.Repeat("a", 4097), strings.Repeat("照", 1366), "a\x00b", "a\nb", "a\u0085b", string([]byte{0xff})} {
		if validSystemCloudIdentifier(value) {
			t.Fatal("invalid locator accepted")
		}
	}
	for _, value := range []string{strings.Repeat("a", 4096), "云标识/+= opaque"} {
		if !validSystemCloudIdentifier(value) {
			t.Fatal("valid locator rejected")
		}
	}
	if systemCopyID("daily_jpeg_v1", "opaque") == systemCopyID("restored_original", "opaque") {
		t.Fatal("kinds alias")
	}
}

func TestMySQLSystemCopiesRestartConcurrentCapacityAndIsolation(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, version := f.login.AccountID, f.version.VersionID
	input := SystemCopyInput{f.version.ManifestDigest, "daily_jpeg_v1", "synthetic-system-云标识"}
	var beforeUsed, beforeReserved, beforeSequence int64
	if err := f.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&beforeUsed, &beforeReserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT MAX(sequence_id) FROM album_library_changes WHERE owner_id=?`, owner).Scan(&beforeSequence); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.repo.RememberSystemCopy(ctx, owner, version, input, *f.now)
			outcomes <- err
		}()
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`DELETE FROM schema_migrations WHERE name='0074_album_system_copy_locators.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, f.db); err != nil {
			t.Fatal("unrecorded DDL replay", err)
		}
	}
	restarted := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return *f.now })
	page, err := restarted.SystemCopies(ctx, owner, version, *f.now)
	if err != nil || len(page.Copies) != 1 || page.ManifestDigest != input.ManifestDigest || page.Copies[0].SystemCloudIdentifier != input.SystemCloudIdentifier {
		t.Fatal("restart replay", page, err)
	}
	if _, err := restarted.SystemCopies(ctx, uuid.NewString(), version, *f.now); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign read", err)
	}
	if _, err := restarted.RememberSystemCopy(ctx, owner, uuid.NewString(), input, *f.now); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing version", err)
	}
	changed := input
	changed.ManifestDigest = strings.Repeat("0", 64)
	if _, err := restarted.RememberSystemCopy(ctx, owner, version, changed, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("digest changed", err)
	}
	changed = input
	changed.Kind = "daily_mp4_1080_v1"
	if _, err := restarted.RememberSystemCopy(ctx, owner, version, changed, *f.now); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal("media kind changed", err)
	}
	for i := range 31 {
		changed = input
		changed.SystemCloudIdentifier = fmt.Sprint("copy-", i)
		if _, err := restarted.RememberSystemCopy(ctx, owner, version, changed, *f.now); err != nil {
			t.Fatal(err)
		}
	}
	// A full page still permits exact retries; concurrent new hints cannot exceed the cap.
	if _, err := restarted.RememberSystemCopy(ctx, owner, version, input, *f.now); err != nil {
		t.Fatal("full retry", err)
	}
	outcomes = make(chan error, 5)
	for i := range 5 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := input
			c.SystemCloudIdentifier = fmt.Sprint("overflow-", i)
			_, err := restarted.RememberSystemCopy(ctx, owner, version, c, *f.now)
			outcomes <- err
		}(i)
	}
	wg.Wait()
	close(outcomes)
	for err := range outcomes {
		if !errors.Is(err, ErrConflict) {
			t.Fatal("capacity", err)
		}
	}
	page, err = restarted.SystemCopies(ctx, owner, version, *f.now)
	if err != nil || len(page.Copies) != 32 {
		t.Fatal("bounded list", err)
	}
	locator := systemCopyID(input.Kind, input.SystemCloudIdentifier)
	for range 2 {
		if err := restarted.ForgetSystemCopy(ctx, owner, version, locator, *f.now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restarted.RememberSystemCopy(ctx, owner, version, input, *f.now); err != nil {
		t.Fatal("capacity not reclaimed", err)
	}
	var used, reserved, sequence int64
	if err := f.db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&used, &reserved); err != nil || used != beforeUsed || reserved != beforeReserved {
		t.Fatal("quota mutated", err)
	}
	if err := f.db.QueryRow(`SELECT MAX(sequence_id) FROM album_library_changes WHERE owner_id=?`, owner).Scan(&sequence); err != nil || sequence != beforeSequence {
		t.Fatal("publication mutated", err)
	}
	current, err := f.repo.AssetVersion(ctx, owner, version)
	if err != nil || current.ManifestDigest != f.version.ManifestDigest {
		t.Fatal("original proof mutated", err)
	}
}

func TestMySQLSystemCopiesHTTPTrustBoundary(t *testing.T) {
	f := newSharingFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", f.login.AccountID}}, f.uploads)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base := "https://albums.example/v1/albums/library/versions/" + f.version.VersionID + "/system-copies"
	input := SystemCopyInput{f.version.ManifestDigest, "daily_jpeg_v1", "synthetic-opaque"}
	data, _ := json.Marshal(input)
	request := func(method, url, body string, host string, kind string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		if host != "" {
			r.Host = host
		}
		if kind != "" {
			r.Header.Set("Content-Type", kind)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		return out
	}
	for _, method := range []string{"POST", "GET"} {
		body := ""
		schema := "SystemCopyList"
		if method == "POST" {
			body = string(data)
			schema = "SystemCopy"
		}
		out := request(method, base, body, "", "application/json")
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body.String())
		}
		var value any
		if json.Unmarshal(out.Body.Bytes(), &value) != nil {
			t.Fatal("invalid JSON")
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{}`, `null`, `[]`, string(data) + `{}`, strings.Replace(string(data), `"kind":`, `"kind":"restored_original","kind":`, 1), strings.Replace(string(data), `"kind":`, `"owner":"someone","kind":`, 1), strings.Replace(string(data), `"synthetic-opaque"`, `null`, 1), strings.Replace(string(data), `"synthetic-opaque"`, `"a\u0000b"`, 1), strings.Replace(string(data), `"synthetic-opaque"`, `"`+strings.Repeat("a", 4097)+`"`, 1), string([]byte{'{', '"', 0xff, '"', ':', '1', '}'})} {
		out := request("POST", base, body, "", "application/json")
		if out.Code != 422 {
			t.Fatal("malformed input", out.Code)
		}
	}
	for _, entry := range []struct {
		method, url, body, host, kind string
		code                          int
	}{
		{"POST", base, string(data), "", "text/plain", 422}, {"POST", base, string(data), "elsewhere.example", "application/json", 421},
		{"GET", base + "?owner=other", "", "", "", 422}, {"POST", base + "?owner=other", string(data), "", "application/json", 422},
		{"GET", base, "{}", "", "", 422}, {"DELETE", base + "/" + strings.Repeat("g", 64), "", "", "", 422},
		{"POST", base, `{"kind":"` + strings.Repeat("a", 32768) + `"}`, "", "application/json", 422},
	} {
		if out := request(entry.method, entry.url, entry.body, entry.host, entry.kind); out.Code != entry.code {
			t.Fatal(entry.method, entry.code, out.Code)
		}
	}
	for _, auth := range []testAccountAuth{{identity: AccountIdentity{"albums", uuid.NewString()}}, {identity: AccountIdentity{"health", f.login.AccountID}}, {err: errors.New("no session")}} {
		other, _ := NewHTTPRouter("albums.example", auth, f.uploads)
		for _, method := range []string{"POST", "GET", "DELETE"} {
			url := base
			body := string(data)
			if method == "DELETE" {
				url += "/" + systemCopyID(input.Kind, input.SystemCloudIdentifier)
			}
			if method != "POST" {
				body = ""
			}
			r := httptest.NewRequest(method, url, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			other.ServeHTTP(out, r)
			if out.Code != 401 && out.Code != 403 && out.Code != 404 {
				t.Fatal("foreign scope", out.Code)
			}
		}
	}
	for range 2 {
		out := request("DELETE", base+"/"+systemCopyID(input.Kind, input.SystemCloudIdentifier), "", "", "")
		if out.Code != http.StatusNoContent || out.Body.Len() != 0 {
			t.Fatal("delete retry", out.Code)
		}
	}
	if f.signer.calls.Load() != 0 {
		t.Fatal("discovery metadata authorized a media read")
	}
	// Removing a hint does not remove its immutable original.
	if _, err := f.repo.AssetVersion(context.Background(), f.login.AccountID, f.version.VersionID); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSystemCopiesLifecycleAndRetention(t *testing.T) {
	for _, fence := range []string{"trash", "deleting", "retention", "quarantine"} {
		t.Run(fence, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx := context.Background()
			owner, version := f.login.AccountID, f.version.VersionID
			input := SystemCopyInput{f.version.ManifestDigest, "restored_original", "synthetic-copy"}
			saved, err := f.repo.RememberSystemCopy(ctx, owner, version, input, *f.now)
			if err != nil {
				t.Fatal(err)
			}
			var query string
			args := []any{owner}
			switch fence {
			case "trash":
				query = `UPDATE album_asset_lifecycle SET state='trashed',trashed_at=NOW(6),trash_expires_at=DATE_ADD(NOW(6),INTERVAL 30 DAY) WHERE owner_id=?`
			case "deleting":
				query = `UPDATE album_storage_accounts SET deletion_requested_at=NOW(6) WHERE owner_id=?`
			case "retention":
				*f.now = f.now.AddDate(1, 1, 0)
				if _, err := f.repo.SystemCopies(ctx, owner, version, *f.now); err != nil {
					t.Fatal("expired export mapping unavailable", err)
				}
				if _, err := f.repo.RememberSystemCopy(ctx, owner, version, input, *f.now); err != nil {
					t.Fatal("expired mapping repair unavailable", err)
				}
				*f.now = f.now.AddDate(1, 0, 0)
			case "quarantine":
				query = `UPDATE album_recovery_state SET quarantined=true,quarantined_at=NOW(6) WHERE singleton_id=1`
				args = nil
			}
			if query != "" {
				if _, err := f.db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.repo.SystemCopies(ctx, owner, version, *f.now); err == nil {
				t.Fatal("read passed fence")
			}
			if _, err := f.repo.RememberSystemCopy(ctx, owner, version, input, *f.now); err == nil {
				t.Fatal("write passed fence")
			}
			if err := f.repo.ForgetSystemCopy(ctx, owner, version, saved.LocatorID, *f.now); err == nil {
				t.Fatal("delete passed fence")
			}
		})
	}
}

// Seed already-deleting fixture versions to exercise cleanup, not API acceptance.
func seedSystemCopyHints(t *testing.T, db *sql.DB, owner string) {
	t.Helper()
	id := "synthetic-cleanup-copy"
	_, err := db.Exec(`INSERT INTO album_system_copy_locators(owner_id,version_id,locator_id,copy_kind,cloud_identifier) SELECT owner_id,version_id,?,'restored_original',? FROM album_asset_versions WHERE owner_id=?`, systemCopyID("restored_original", id), id, owner)
	if err != nil {
		t.Fatal(err)
	}
}
