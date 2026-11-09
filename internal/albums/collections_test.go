package albums

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func createTestCollection(t *testing.T, f sharingFixture) (string, CloudCollection) {
	t.Helper()
	id := uuid.NewString()
	v, err := f.repo.SaveCollection(context.Background(), f.login.AccountID, id, CloudCollectionInput{uuid.NewString(), 0, "日常的小事", false}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	return id, v
}
func TestCollectionNameValidation(t *testing.T) {
	for _, v := range []string{"", " ", " 前空格", "后空格 ", "换\n行", "tab\t", "\x00", string([]byte{0xff}), strings.Repeat("中", 81)} {
		if validCollectionName(v, false) {
			t.Fatalf("invalid name accepted: %q", v)
		}
	}
	for _, v := range []string{"旅行 📷", strings.Repeat("中", 80)} {
		if !validCollectionName(v, false) {
			t.Fatal("valid name rejected")
		}
	}
	if !validCollectionName("", true) || validCollectionName("仍保留名称", true) {
		t.Fatal("deletion must clear title")
	}
}
func TestMySQLCollectionsPersistenceRemovalAndDeletionDoNotChangeMedia(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, asset := f.login.AccountID, f.version.AssetID
	state := cloudState(t, f)
	used, reserved := photoJournalBalance(t, f.db, owner)
	id, first := createTestCollection(t, f)
	initial, err := f.repo.CollectionMember(ctx, owner, id, asset, *f.now)
	if err != nil || initial.Revision != 0 || initial.Present || initial.UpdatedAt != nil {
		t.Fatal(initial, err)
	}
	input := CloudCollectionMemberInput{uuid.NewString(), 0, true}
	member, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, input, *f.now)
	if err != nil || !member.Present || member.Revision != 1 {
		t.Fatal(member, err)
	}
	again, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, input, *f.now)
	if err != nil || !reflect.DeepEqual(member, again) {
		t.Fatal("replay duplicated", again, err)
	}
	changed := input
	changed.Present = false
	if _, err = f.repo.SaveCollectionMember(ctx, owner, id, asset, changed, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("changed replay", err)
	}
	rename := CloudCollectionInput{uuid.NewString(), first.Revision, "周末", false}
	renamed, err := f.repo.SaveCollection(ctx, owner, id, rename, *f.now)
	if err != nil || renamed.Sequence <= first.Sequence {
		t.Fatal(renamed, err)
	}
	if got, err := f.repo.CollectionMember(ctx, owner, id, asset, *f.now); err != nil || !reflect.DeepEqual(got, member) {
		t.Fatal("rename removed member", got, err)
	}
	againCollection, err := f.repo.SaveCollection(ctx, owner, id, rename, *f.now)
	if err != nil || !reflect.DeepEqual(againCollection, renamed) {
		t.Fatal("metadata replay", againCollection, err)
	}
	if !reflect.DeepEqual(state, cloudState(t, f)) {
		t.Fatal("grouping altered lifecycle")
	}
	groupedUsed, groupedReserved := photoJournalBalance(t, f.db, owner)
	if used != groupedUsed || reserved != groupedReserved {
		t.Fatal("grouping changed quota")
	}
	next := lifecycleUpload(t, f, "collection-next-backup")
	if _, err := completeLifecycleUpload(t, f, next); err != nil {
		t.Fatal(err)
	}
	state = cloudState(t, f)
	used, reserved = photoJournalBalance(t, f.db, owner)
	reopened := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now)
	if got, err := reopened.CollectionMember(ctx, owner, id, asset, *f.now); err != nil || !reflect.DeepEqual(got, member) {
		t.Fatal("new backup lost membership", got, err)
	}
	removed, err := reopened.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 1, false}, *f.now)
	if err != nil || removed.Present || removed.Sequence <= member.Sequence {
		t.Fatal(removed, err)
	}
	if _, err = reopened.SaveCollectionMember(ctx, owner, id, asset, input, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("stale retry resurrected member", err)
	}
	page, err := reopened.CollectionMembers(ctx, owner, member.Sequence, 1, *f.now)
	if err != nil || len(page.Members) != 1 || page.Members[0].Present || page.NextSequence != removed.Sequence || page.HasMore {
		t.Fatal(page, err)
	}
	del := CloudCollectionInput{uuid.NewString(), renamed.Revision, "", true}
	deleted, err := reopened.SaveCollection(ctx, owner, id, del, *f.now)
	if err != nil || !deleted.Deleted {
		t.Fatal(deleted, err)
	}
	if got, err := reopened.SaveCollection(ctx, owner, id, del, *f.now); err != nil || !reflect.DeepEqual(got, deleted) {
		t.Fatal("delete replay", err)
	}
	if _, err := reopened.SaveCollection(ctx, owner, id, CloudCollectionInput{uuid.NewString(), deleted.Revision, "复活", false}, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted collection revived", err)
	}
	if _, err := reopened.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 2, true}, *f.now); err == nil {
		t.Fatal("member added to deleted collection")
	}
	collections, err := reopened.Collections(ctx, owner, renamed.Sequence, 100, *f.now)
	if err != nil || len(collections.Collections) != 1 || !collections.Collections[0].Deleted {
		t.Fatal("tombstone missing", collections, err)
	}
	// Compare against state after a deliberately separate second backup, not against its earlier revision.
	if !reflect.DeepEqual(state, cloudState(t, f)) {
		t.Fatal("collection altered lifecycle")
	}
	v, err := f.repo.AssetVersion(ctx, owner, f.version.VersionID)
	if err != nil || !reflect.DeepEqual(v, f.version) {
		t.Fatal("immutable original changed", err)
	}
	afterUsed, afterReserved := photoJournalBalance(t, f.db, owner)
	if afterUsed != used || reserved != afterReserved || f.signer.calls.Load() != 0 {
		t.Fatal("collection changed media quota or issued grants")
	}
}
func TestMySQLCollectionsConcurrentCASAndRollback(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner := f.login.AccountID
	id, first := createTestCollection(t, f)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.repo.SaveCollection(ctx, owner, id, CloudCollectionInput{uuid.NewString(), first.Revision, "并发", false}, *f.now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatal(ok, conflict)
	}
	before, err := f.repo.Collection(ctx, owner, id, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`ALTER TABLE album_collections ADD CONSTRAINT controlled_collection_failure CHECK(revision<>3)`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.SaveCollection(ctx, owner, id, CloudCollectionInput{uuid.NewString(), before.Revision, "失败", false}, *f.now); err == nil {
		t.Fatal("controlled failure missing")
	}
	if after, err := f.repo.Collection(ctx, owner, id, *f.now); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rollback lost collection", after, err)
	}
	member, err := f.repo.SaveCollectionMember(ctx, owner, id, f.version.AssetID, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`ALTER TABLE album_collection_members ADD CONSTRAINT controlled_member_failure CHECK(revision<>2)`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.SaveCollectionMember(ctx, owner, id, f.version.AssetID, CloudCollectionMemberInput{uuid.NewString(), 1, false}, *f.now); err == nil {
		t.Fatal("controlled membership failure missing")
	}
	if after, err := f.repo.CollectionMember(ctx, owner, id, f.version.AssetID, *f.now); err != nil || !reflect.DeepEqual(member, after) {
		t.Fatal("rollback lost member", after, err)
	}
}
func TestMySQLCollectionsCursorEditsAndOwnerIsolation(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner := f.login.AccountID
	id, first := createTestCollection(t, f)
	second, _ := createTestCollection(t, f)
	p, err := f.repo.Collections(ctx, owner, 0, 1, *f.now)
	if err != nil || len(p.Collections) != 1 || !p.HasMore {
		t.Fatal(p, err)
	}
	changed, err := f.repo.SaveCollection(ctx, owner, id, CloudCollectionInput{uuid.NewString(), 1, "分页期间修改", false}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]CloudCollection{}
	after := p.NextSequence
	for i := 0; i < 3; i++ {
		p, err = f.repo.Collections(ctx, owner, after, 1, *f.now)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Collections {
			seen[v.CollectionID] = v
		}
		after = p.NextSequence
		if !p.HasMore {
			break
		}
	}
	if len(seen) != 2 || seen[id].Sequence != changed.Sequence || seen[second].Sequence <= first.Sequence {
		t.Fatal("edit lost behind cursor", seen)
	}
	// Use a valid second account in the same database, not an unknown owner or separate empty DB.
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewMySQLAccounts(albumDevelopmentAudience, f.db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: freshAppleIdentity(uuid.NewString()), RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	foreign := other.AccountID
	if _, err := f.repo.ApplySubscription(ctx, foreign, 0, VerifiedAlbumSubscription{uuid.NewString(), uuid.NewString(), Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(1, 0, 0), *f.now, true}, *f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Collection(ctx, foreign, id, *f.now); err == nil {
		t.Fatal("foreign collection read")
	}
	foreignID, err := f.repo.SaveCollection(ctx, foreign, uuid.NewString(), CloudCollectionInput{uuid.NewString(), 0, "另一个账户", false}, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.SaveCollectionMember(ctx, foreign, foreignID.CollectionID, f.version.AssetID, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now); err == nil {
		t.Fatal("foreign asset added")
	}
	page, err := f.repo.Collections(ctx, foreign, 0, 100, *f.now)
	if err != nil || len(page.Collections) != 1 || page.Collections[0].CollectionID != foreignID.CollectionID {
		t.Fatal("foreign feed", page, err)
	}
	for _, input := range []CloudCollectionInput{{uuid.NewString(), math.MaxInt64, "x", false}, {uuid.NewString(), -1, "x", false}, {"bad", 0, "x", false}} {
		if _, err := f.repo.SaveCollection(ctx, owner, uuid.NewString(), input, *f.now); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal("invalid revision", err)
		}
	}
}
func TestMySQLCollectionsAccountFencesAndTrash(t *testing.T) {
	for _, fence := range []string{"trash", "deleting", "retention", "quarantine"} {
		t.Run(fence, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx := context.Background()
			owner, asset := f.login.AccountID, f.version.AssetID
			id, _ := createTestCollection(t, f)
			if _, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now); err != nil {
				t.Fatal(err)
			}
			query := ""
			args := []any{owner}
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
				if _, err := f.repo.Collection(ctx, owner, id, *f.now); err != nil {
					t.Fatal("export period blocked", err)
				}
				*f.now = f.now.AddDate(1, 0, 0)
			}
			if query != "" {
				if _, err := f.db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.repo.CollectionMember(ctx, owner, id, asset, *f.now); err == nil {
				t.Fatal("member passed fence")
			}
			if _, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 1, false}, *f.now); err == nil {
				t.Fatal("write passed fence")
			}
			page, err := f.repo.CollectionMembers(ctx, owner, 0, 100, *f.now)
			if fence == "trash" {
				if err != nil || len(page.Members) != 1 || !page.Members[0].Present {
					t.Fatal("trash lost membership", page, err)
				}
			} else {
				if err == nil {
					t.Fatal("feed passed fence")
				}
				if _, err := f.repo.SaveCollection(ctx, owner, id, CloudCollectionInput{uuid.NewString(), 1, "x", false}, *f.now); err == nil {
					t.Fatal("metadata passed fence")
				}
			}
		})
	}
}
func TestMySQLCollectionsHTTPContractAndStrictBoundary(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	id, _ := createTestCollection(t, f)
	limiter := &loginHTTPLimiter{}
	auth, _ := NewMySQLSessions(f.db, f.uploads.now)
	options := &LoginHTTPOptions{LibraryLimiter: limiter}
	router, err := newHTTPRouter("albums.example", auth, f.uploads, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	missing := "https://albums.example/v1/albums/collections/" + uuid.NewString()
	if w := send("GET", missing, "", f.session.Token); w.Code != 404 || !strings.Contains(w.Body.String(), "asset_version_not_found") {
		t.Fatal("missing collection is not an empty revision baseline", w.Code, w.Body.String())
	}
	if w := send("GET", missing+"/members/"+f.version.AssetID, "", f.session.Token); w.Code != 404 {
		t.Fatal("missing parent", w.Code)
	}
	createBody, _ := json.Marshal(CloudCollectionInput{uuid.NewString(), 0, "新相册", false})
	if w := send("PUT", missing, string(createBody), f.session.Token); w.Code != 200 {
		t.Fatal("first create", w.Code, w.Body.String())
	}
	base := "https://albums.example/v1/albums/collections/" + id
	member := base + "/members/" + f.version.AssetID
	body, _ := json.Marshal(CloudCollectionInput{uuid.NewString(), 1, "周末", false})
	mb, _ := json.Marshal(CloudCollectionMemberInput{uuid.NewString(), 0, true})
	for _, e := range []struct{ method, path, body, schema string }{{"GET", base, "", "CloudCollection"}, {"PUT", base, string(body), "CloudCollection"}, {"GET", member, "", "CloudCollectionMember"}, {"PUT", member, string(mb), "CloudCollectionMember"}, {"GET", "https://albums.example/v1/albums/collections?after=0&limit=1", "", "CloudCollectionPage"}, {"GET", "https://albums.example/v1/albums/collection-members?after=0&limit=1", "", "CloudCollectionMemberPage"}} {
		w := send(e.method, e.path, e.body, f.session.Token)
		var v any
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := doc.Components.Schemas[e.schema].Value.VisitJSON(v); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct{ path, body string }{{base, string(body)}, {member, string(mb)}} {
		for _, bad := range []string{"null", "{}", "[]", entry.body + "{}", strings.Replace(entry.body, `"requestID":`, `"owner":"foreign","requestID":`, 1), strings.Replace(entry.body, `"expectedRevision":`, `"expectedRevision":null,"expectedRevision":`, 1), strings.Replace(entry.body, `"requestID":`, `"requestID":null,"requestID":`, 1), strings.Repeat(" ", 2049), string([]byte{0xff})} {
			if w := send("PUT", entry.path, bad, f.session.Token); w.Code != 422 {
				t.Fatal("invalid body accepted", w.Code, w.Body.String())
			}
		}
	}
	for _, p := range []string{base + "?owner=foreign", member + "?owner=foreign", "https://albums.example/v1/albums/collections?owner=foreign", "https://albums.example/v1/albums/collection-members?owner=foreign"} {
		if w := send("GET", p, "", f.session.Token); w.Code != 422 {
			t.Fatal("query injection", w.Code)
		}
	}
	if w := send("GET", base, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous", w.Code)
	}
	if w := send("GET", strings.Replace(base, "albums.example", "health.example", 1), "", f.session.Token); w.Code != 421 {
		t.Fatal("foreign host", w.Code)
	}
	limiter.delay = time.Second
	if w := send("GET", base, "", f.session.Token); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("rate limit", w.Code)
	}
	if limiter.operation != "library-lifecycle" || limiter.address != "192.0.2.1" {
		t.Fatal("wrong limiter identity")
	}
	if _, err := f.repo.Collection(ctx, f.login.AccountID, id, *f.now); err != nil || f.signer.calls.Load() != 0 {
		t.Fatal("metadata authorized media", err)
	}
}

func seedCollections(t *testing.T, db *sql.DB, owner string) {
	t.Helper()
	if _, err := db.Exec(`INSERT IGNORE INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, owner, owner); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'synthetic private collection',false,UTC_TIMESTAMP(6))`, owner, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_collection_members(owner_id,collection_id,asset_id,revision,request_id,present,updated_at) SELECT owner_id,?,asset_id,1,UUID(),true,UTC_TIMESTAMP(6) FROM album_asset_lifecycle WHERE owner_id=?`, id, owner); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLCollectionMemberCursorEditsAndConcurrentCAS(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, asset := f.login.AccountID, f.version.AssetID
	first, _ := createTestCollection(t, f)
	second, _ := createTestCollection(t, f)
	for _, id := range []string{first, second} {
		if _, err := f.repo.SaveCollectionMember(ctx, owner, id, asset, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.repo.CollectionMembers(ctx, owner, 0, 1, *f.now)
	if err != nil || len(page.Members) != 1 || !page.HasMore {
		t.Fatal(page, err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.repo.SaveCollectionMember(ctx, owner, first, asset, CloudCollectionMemberInput{uuid.NewString(), 1, false}, *f.now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatal(ok, conflicts)
	}
	seen := map[string]CloudCollectionMember{}
	after := page.NextSequence
	for i := 0; i < 3; i++ {
		page, err = f.repo.CollectionMembers(ctx, owner, after, 1, *f.now)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Members {
			seen[v.CollectionID] = v
		}
		after = page.NextSequence
		if !page.HasMore {
			break
		}
	}
	if len(seen) != 2 || seen[first].Present || !seen[second].Present {
		t.Fatal("membership edit missed behind cursor", seen)
	}
}
