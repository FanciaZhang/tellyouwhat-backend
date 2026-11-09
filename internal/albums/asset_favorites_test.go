package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func seedAssetFavorites(t *testing.T, db *sql.DB, owner string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO album_asset_favorites(owner_id,asset_id,revision,request_id,favorite,updated_at) SELECT owner_id,asset_id,1,UUID(),true,UTC_TIMESTAMP(6) FROM album_asset_lifecycle WHERE owner_id=?`, owner)
	if err != nil {
		t.Fatal(err)
	}
}
func TestMySQLAssetFavoriteReplayResetAndVersionIndependence(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, asset := f.login.AccountID, f.version.AssetID
	before := cloudState(t, f)
	used, reserved := photoJournalBalance(t, f.db, owner)
	initial, err := f.repo.AssetFavorite(ctx, owner, asset, *f.now)
	if err != nil || initial.Revision != 0 || initial.Sequence != 0 || initial.UpdatedAt != nil || initial.Favorite {
		t.Fatal(initial, err)
	}
	input := AssetFavoriteInput{uuid.NewString(), 0, true}
	first, err := f.repo.SaveAssetFavorite(ctx, owner, asset, input, *f.now)
	if err != nil || first.Revision != 1 || !first.Favorite || first.Sequence < 1 {
		t.Fatal(first, err)
	}
	again, err := f.repo.SaveAssetFavorite(ctx, owner, asset, input, *f.now)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatal("replay changed state", again, err)
	}
	changed := input
	changed.Favorite = false
	if _, err := f.repo.SaveAssetFavorite(ctx, owner, asset, changed, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("request reused for new intent", err)
	}
	if !reflect.DeepEqual(before, cloudState(t, f)) {
		t.Fatal("favorite changed lifecycle")
	}
	version, err := f.repo.AssetVersion(ctx, owner, f.version.VersionID)
	if err != nil || !reflect.DeepEqual(version, f.version) {
		t.Fatal("immutable version changed", err)
	}
	afterUsed, afterReserved := photoJournalBalance(t, f.db, owner)
	if used != afterUsed || reserved != afterReserved || f.signer.calls.Load() != 0 {
		t.Fatal("favorite altered quota or authorized media")
	}
	second := lifecycleUpload(t, f, "favorite-next-backup")
	if _, err := completeLifecycleUpload(t, f, second); err != nil {
		t.Fatal(err)
	}
	reopened := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now)
	got, err := reopened.AssetFavorite(ctx, owner, asset, *f.now)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatal("new version lost explicit choice", got, err)
	}
	reset, err := reopened.SaveAssetFavorite(ctx, owner, asset, AssetFavoriteInput{uuid.NewString(), 1, false}, *f.now)
	if err != nil || reset.Revision != 2 || reset.Favorite || reset.Sequence <= first.Sequence {
		t.Fatal("false reset lost", reset, err)
	}
	page, err := reopened.AssetFavorites(ctx, owner, first.Sequence, 1, *f.now)
	if err != nil || len(page.Favorites) != 1 || page.Favorites[0].Favorite || page.NextSequence != reset.Sequence || page.HasMore {
		t.Fatal(page, err)
	}
	if _, err := reopened.SaveAssetFavorite(ctx, owner, asset, input, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("old replay overwrote reset", err)
	}
}
func TestMySQLAssetFavoriteSnapshotDefaultDoesNotAllocateChoice(t *testing.T) {
	base := newSharingFixture(t).version.Manifest
	base.Metadata = &AssetMetadata{PixelWidth: 100, PixelHeight: 100, Favorite: true}
	f := newSharingManifestFixture(t, base)
	ctx := context.Background()
	value, err := f.repo.AssetFavorite(ctx, f.login.AccountID, f.version.AssetID, *f.now)
	if err != nil || !value.Favorite || value.Revision != 0 {
		t.Fatal(value, err)
	}
	page, err := f.repo.AssetFavorites(ctx, f.login.AccountID, 0, 100, *f.now)
	if err != nil || len(page.Favorites) != 0 || page.NextSequence != 0 {
		t.Fatal("snapshot created intent", page, err)
	}
	value, err = f.repo.SaveAssetFavorite(ctx, f.login.AccountID, f.version.AssetID, AssetFavoriteInput{uuid.NewString(), 0, false}, *f.now)
	if err != nil || value.Favorite || value.Revision != 1 {
		t.Fatal("explicit false ignored", value, err)
	}
}
func TestMySQLAssetFavoriteConcurrentCASAndRollback(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	var group sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < 6; i++ {
		group.Go(func() {
			_, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, f.version.AssetID, AssetFavoriteInput{uuid.NewString(), 0, true}, *f.now)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if winners != 1 {
		t.Fatal("concurrent overwrite", winners)
	}
	first, err := f.repo.AssetFavorite(ctx, f.login.AccountID, f.version.AssetID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_asset_favorites ADD CONSTRAINT controlled_favorite_failure CHECK(revision<>2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, f.version.AssetID, AssetFavoriteInput{uuid.NewString(), 1, false}, *f.now); err == nil {
		t.Fatal("write failure not reported")
	}
	after, err := f.repo.AssetFavorite(ctx, f.login.AccountID, f.version.AssetID, *f.now)
	if err != nil || !reflect.DeepEqual(first, after) {
		t.Fatal("failed write lost previous choice", after, err)
	}
}
func TestMySQLAssetFavoriteCursorObservesEditsBetweenPages(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	assets := []string{f.version.AssetID}
	for i := 0; i < 2; i++ {
		manifest := f.version.Manifest
		manifest.AssetID = uuid.NewString()
		manifest.SourceRevision = uuid.NewString()
		u, err := f.uploads.Create(ctx, f.login.AccountID, uuid.NewString(), manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := completeLifecycleUpload(t, f, u); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, manifest.AssetID)
	}
	for _, asset := range assets {
		if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, asset, AssetFavoriteInput{uuid.NewString(), 0, true}, *f.now); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.repo.AssetFavorites(ctx, f.login.AccountID, 0, 1, *f.now)
	if err != nil || len(page.Favorites) != 1 || page.Favorites[0].AssetID != assets[0] || !page.HasMore {
		t.Fatal(page, err)
	}
	if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, assets[0], AssetFavoriteInput{uuid.NewString(), 1, false}, *f.now); err != nil {
		t.Fatal(err)
	}
	after := page.NextSequence
	for i, asset := range []string{assets[1], assets[2], assets[0]} {
		page, err = f.repo.AssetFavorites(ctx, f.login.AccountID, after, 1, *f.now)
		if err != nil || len(page.Favorites) != 1 || page.Favorites[0].AssetID != asset || page.NextSequence <= after || page.HasMore != (i < 2) {
			t.Fatal("cursor missed edit", page, err)
		}
		after = page.NextSequence
	}
	page, err = f.repo.AssetFavorites(ctx, f.login.AccountID, after, 1, *f.now)
	if err != nil || len(page.Favorites) != 0 || page.NextSequence != after {
		t.Fatal("unstable empty cursor", page, err)
	}
}
func TestMySQLAssetFavoriteLifecycleRetentionAndOwnerFences(t *testing.T) {
	for _, fence := range []string{"trash", "deleting", "retention", "quarantine"} {
		t.Run(fence, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx := context.Background()
			input := AssetFavoriteInput{uuid.NewString(), 0, true}
			if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, f.version.AssetID, input, *f.now); err != nil {
				t.Fatal(err)
			}
			if _, err := f.repo.AssetFavorite(ctx, uuid.NewString(), f.version.AssetID, *f.now); err == nil {
				t.Fatal("foreign read")
			}
			if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, uuid.NewString(), input, *f.now); err == nil {
				t.Fatal("unpublished asset write")
			}
			query := ""
			args := []any{f.login.AccountID}
			switch fence {
			case "trash":
				query = `UPDATE album_asset_lifecycle SET state='trashed',trashed_at=NOW(6),trash_expires_at=DATE_ADD(NOW(6),INTERVAL 30 DAY) WHERE owner_id=?`
			case "deleting":
				query = `UPDATE album_storage_accounts SET deletion_requested_at=NOW(6) WHERE owner_id=?`
			case "quarantine":
				query = `UPDATE album_recovery_state SET quarantined=true,quarantined_at=NOW(6) WHERE singleton_id=1`
				args = nil
			case "retention":
				*f.now = f.now.AddDate(1, 1, 0)
				if _, err := f.repo.AssetFavorite(ctx, f.login.AccountID, f.version.AssetID, *f.now); err != nil {
					t.Fatal("export period unavailable", err)
				}
				*f.now = f.now.AddDate(1, 0, 0)
			}
			if query != "" {
				if _, err := f.db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.repo.AssetFavorite(ctx, f.login.AccountID, f.version.AssetID, *f.now); err == nil {
				t.Fatal("read passed fence")
			}
			input.RequestID = uuid.NewString()
			input.ExpectedRevision = 1
			if _, err := f.repo.SaveAssetFavorite(ctx, f.login.AccountID, f.version.AssetID, input, *f.now); err == nil {
				t.Fatal("write passed fence")
			}
			page, err := f.repo.AssetFavorites(ctx, f.login.AccountID, 0, 100, *f.now)
			if fence == "trash" {
				if err != nil || len(page.Favorites) != 1 {
					t.Fatal("trash choice lost", page, err)
				}
			} else if err == nil {
				t.Fatal("feed passed account fence")
			}
		})
	}
}
func TestMySQLAssetFavoriteHTTPStrictBodyAndContract(t *testing.T) {
	f := newSharingFixture(t)
	auth, _ := NewMySQLSessions(f.db, f.uploads.now)
	router, err := NewHTTPRouter("albums.example", auth, f.uploads)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base := "https://albums.example/v1/albums/library/assets/" + f.version.AssetID + "/favorite"
	input := AssetFavoriteInput{uuid.NewString(), 0, false}
	data, _ := json.Marshal(input)
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, entry := range []struct{ method, path, body, schema string }{{"GET", base, "", "AssetFavorite"}, {"PUT", base, string(data), "AssetFavorite"}, {"GET", "https://albums.example/v1/albums/favorites?after=0&limit=1", "", "AssetFavoritePage"}} {
		w := request(entry.method, entry.path, entry.body, f.session.Token)
		var value any
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &value) != nil || doc.Components.Schemas[entry.schema].Value.VisitJSON(value) != nil {
			t.Fatal("HTTP contract", w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`null`, `{}`, `[]`, string(data) + `{}`, strings.Replace(string(data), `"favorite":false`, `"favorite":null`, 1), strings.Replace(string(data), `"favorite":false`, `"favorite":false,"favorite":true`, 1), strings.Replace(string(data), `"favorite":false`, `"favorite":"false"`, 1), strings.Replace(string(data), `"requestID":`, `"owner":"foreign","requestID":`, 1), strings.Replace(string(data), `"expectedRevision":0`, `"expectedRevision":null`, 1), strings.Repeat(" ", 1025), string([]byte{0xff})} {
		if w := request("PUT", base, body, f.session.Token); w.Code != 422 {
			t.Fatal("invalid body accepted", w.Code)
		}
	}
	for _, path := range []string{base + "?owner=foreign", "https://albums.example/v1/albums/favorites?owner=foreign"} {
		if w := request("GET", path, "", f.session.Token); w.Code != 422 {
			t.Fatal("query injection", w.Code)
		}
	}
	if w := request("GET", "https://albums.example/v1/albums/favorites?after=0&after=1", "", f.session.Token); w.Code != 400 {
		t.Fatal("duplicate cursor", w.Code)
	}
	if w := request("GET", base, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous read", w.Code)
	}
	if w := request("GET", strings.Replace(base, "albums.example", "health.example", 1), "", f.session.Token); w.Code != http.StatusMisdirectedRequest {
		t.Fatal("foreign host", w.Code)
	}
	if f.signer.calls.Load() != 0 {
		t.Fatal("favorite authorized media")
	}
}
