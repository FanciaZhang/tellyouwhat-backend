package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestContentCorrectionCategoriesAreExplicitBoundedSets(t *testing.T) {
	input := ContentCategories{[]string{"dogs", "cats"}, []string{"people"}}
	got, err := canonicalContentCategories(input)
	if err != nil || !slices.Equal(got.Included, []string{"cats", "dogs"}) || input.Included[0] != "dogs" {
		t.Fatal(got, err, "caller choices mutated")
	}
	for _, invalid := range []ContentCategories{{nil, []string{}}, {[]string{}, nil}, {[]string{"dogs", "dogs"}, []string{}}, {[]string{"dogs"}, []string{"dogs"}}, {[]string{"person"}, []string{}}, {[]string{"DOGS"}, []string{}}, {[]string{strings.Repeat("a", 4096)}, []string{}}} {
		if _, err := canonicalContentCategories(invalid); err == nil {
			t.Fatal("invalid choice accepted")
		}
	}
	if _, err := canonicalContentCategories(ContentCategories{[]string{}, []string{}}); err != nil {
		t.Fatal("explicit reset rejected", err)
	}
}

func correctionInput(f sharingFixture) ContentCorrectionInput {
	return ContentCorrectionInput{RequestID: uuid.NewString(), ManifestDigest: f.version.ManifestDigest,
		ContentCategories: ContentCategories{[]string{"dogs", "cats"}, []string{"people"}}}
}

func TestMySQLContentCorrectionCASReplayResetRestart(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	owner, version := f.login.AccountID, f.version.VersionID
	empty, err := f.repo.ContentCorrection(ctx, owner, version, *f.now)
	if err != nil || empty.Revision != 0 || empty.Sequence != 0 || empty.Included == nil || empty.Excluded == nil || empty.UpdatedAt != nil {
		t.Fatal("missing default", empty, err)
	}
	input := correctionInput(f)
	first, err := f.repo.SaveContentCorrection(ctx, owner, version, input, *f.now)
	if err != nil || first.Revision != 1 || first.Sequence <= 0 || !slices.Equal(first.Included, []string{"cats", "dogs"}) {
		t.Fatal(first, err)
	}
	input.Included = []string{"cats", "dogs"}
	again, err := f.repo.SaveContentCorrection(ctx, owner, version, input, *f.now)
	if err != nil || again.Revision != first.Revision || again.Sequence != first.Sequence {
		t.Fatal("replay allocated another edit", again, err)
	}
	changed := input
	changed.Excluded = []string{}
	if _, err := f.repo.SaveContentCorrection(ctx, owner, version, changed, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("request ID reused with changed choices", err)
	}
	reset := ContentCorrectionInput{RequestID: uuid.NewString(), ExpectedRevision: 1, ManifestDigest: input.ManifestDigest, ContentCategories: ContentCategories{[]string{}, []string{}}}
	cleared, err := f.repo.SaveContentCorrection(ctx, owner, version, reset, *f.now)
	if err != nil || cleared.Revision != 2 || cleared.Sequence <= first.Sequence || cleared.Included == nil || len(cleared.Included) != 0 {
		t.Fatal("reset tombstone missing", cleared, err)
	}
	if _, err := f.repo.SaveContentCorrection(ctx, owner, version, input, *f.now); !errors.Is(err, ErrConflict) {
		t.Fatal("old replay overwrote newer reset", err)
	}
	restarted := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox", f.uploads.now)
	page, err := restarted.ContentCorrections(ctx, owner, first.Sequence, 1, *f.now)
	if err != nil || len(page.Corrections) != 1 || page.HasMore || page.NextSequence != cleared.Sequence || page.Corrections[0].Revision != 2 {
		t.Fatal("restart/cursor lost reset", page, err)
	}
	page, err = restarted.ContentCorrections(ctx, owner, page.NextSequence, 1, *f.now)
	if err != nil || len(page.Corrections) != 0 || page.NextSequence != cleared.Sequence {
		t.Fatal("unstable empty cursor", page, err)
	}
	if _, err := restarted.ContentCorrection(ctx, uuid.NewString(), version, *f.now); err == nil {
		t.Fatal("foreign owner read correction")
	}
	if _, err := restarted.SaveContentCorrection(ctx, owner, uuid.NewString(), input, *f.now); err == nil {
		t.Fatal("unpublished version accepted")
	}
}

func seedContentCorrections(t *testing.T, db *sql.DB, owner string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO album_content_corrections(owner_id,version_id,revision,request_id,correction_json,updated_at)
 SELECT owner_id,version_id,1,UUID(),'{"included":["dogs"],"excluded":[]}',UTC_TIMESTAMP(6) FROM album_asset_versions WHERE owner_id=?`, owner)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMySQLContentCorrectionPaginationObservesEditsBetweenPages(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	versions := []string{f.version.VersionID}
	inputs := []ContentCorrectionInput{correctionInput(f)}
	for _, source := range []string{"correction-page-two", "correction-page-three"} {
		u := lifecycleUpload(t, f, source)
		if _, err := completeLifecycleUpload(t, f, u); err != nil {
			t.Fatal(err)
		}
		input := correctionInput(f)
		input.ManifestDigest = u.Digest
		versions = append(versions, u.ID)
		inputs = append(inputs, input)
	}
	used, reserved := photoJournalBalance(t, f.db, f.login.AccountID)
	for i, version := range versions {
		if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, version, inputs[i], *f.now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.repo.ContentCorrections(ctx, f.login.AccountID, 0, 1, *f.now)
	if err != nil || len(first.Corrections) != 1 || !first.HasMore || first.Corrections[0].VersionID != versions[0] {
		t.Fatal("first page", first, err)
	}
	update := inputs[0]
	update.RequestID, update.ExpectedRevision = uuid.NewString(), 1
	update.ContentCategories = ContentCategories{[]string{}, []string{}}
	if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, versions[0], update, *f.now); err != nil {
		t.Fatal(err)
	}
	after := first.NextSequence
	for i, expected := range []string{versions[1], versions[2], versions[0]} {
		page, err := f.repo.ContentCorrections(ctx, f.login.AccountID, after, 1, *f.now)
		if err != nil || len(page.Corrections) != 1 || page.Corrections[0].VersionID != expected || page.NextSequence <= after || page.HasMore != (i < 2) {
			t.Fatal("edit skipped or cursor reordered", page, err)
		}
		if i == 2 && (page.Corrections[0].Revision != 2 || len(page.Corrections[0].Included) != 0) {
			t.Fatal("reset missed between pages", page)
		}
		after = page.NextSequence
	}
	for _, input := range []ContentCorrectionInput{
		{RequestID: uuid.NewString(), ManifestDigest: strings.Repeat("0", 64), ContentCategories: update.ContentCategories},
		{RequestID: uuid.NewString(), ExpectedRevision: -1, ManifestDigest: f.version.ManifestDigest, ContentCategories: update.ContentCategories},
	} {
		if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, versions[0], input, *f.now); err == nil {
			t.Fatal("unbound or invalid edit accepted")
		}
	}
	if a, b := photoJournalBalance(t, f.db, f.login.AccountID); a != used || b != reserved {
		t.Fatal("classification changed media quota")
	}
}

func TestMySQLContentCorrectionConcurrentCASAndFailedWritePreservePreviousState(t *testing.T) {
	f := newSharingFixture(t)
	ctx := context.Background()
	input := correctionInput(f)
	var group sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, f.version.VersionID, input, *f.now)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("identical replay failed", err)
		}
	}
	first, err := f.repo.ContentCorrection(ctx, f.login.AccountID, f.version.VersionID, *f.now)
	if err != nil || first.Revision != 1 {
		t.Fatal(first, err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_content_corrections ADD CONSTRAINT controlled_correction_failure CHECK(revision<>2)`); err != nil {
		t.Fatal(err)
	}
	input.RequestID = uuid.NewString()
	input.ExpectedRevision = 1
	input.Included = []string{"food"}
	if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, f.version.VersionID, input, *f.now); err == nil {
		t.Fatal("forced insert failure accepted")
	}
	retained, err := f.repo.ContentCorrection(ctx, f.login.AccountID, f.version.VersionID, *f.now)
	if err != nil || retained.Sequence != first.Sequence || !slices.Equal(retained.Included, first.Included) {
		t.Fatal("failed replacement removed old choices", retained, err)
	}
	if _, err := f.db.Exec(`ALTER TABLE album_content_corrections DROP CHECK controlled_correction_failure`); err != nil {
		t.Fatal(err)
	}
	results = make(chan error, 12)
	for range 12 {
		request := input
		request.RequestID = uuid.NewString()
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, f.version.VersionID, request, *f.now)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatal("concurrent edits silently overwrote each other", winners)
	}
}

func TestMySQLContentCorrectionLifecycleAndRetention(t *testing.T) {
	for _, fence := range []string{"trash", "deleting", "retention", "quarantine"} {
		t.Run(fence, func(t *testing.T) {
			f := newSharingFixture(t)
			ctx := context.Background()
			input := correctionInput(f)
			if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, f.version.VersionID, input, *f.now); err != nil {
				t.Fatal(err)
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
				if _, err := f.repo.ContentCorrection(ctx, f.login.AccountID, f.version.VersionID, *f.now); err != nil {
					t.Fatal("export-period correction unavailable", err)
				}
				*f.now = f.now.AddDate(1, 0, 0)
			}
			if query != "" {
				if _, err := f.db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.repo.ContentCorrection(ctx, f.login.AccountID, f.version.VersionID, *f.now); err == nil {
				t.Fatal("version read passed fence")
			}
			input.RequestID = uuid.NewString()
			input.ExpectedRevision = 1
			if _, err := f.repo.SaveContentCorrection(ctx, f.login.AccountID, f.version.VersionID, input, *f.now); err == nil {
				t.Fatal("write passed fence")
			}
			page, err := f.repo.ContentCorrections(ctx, f.login.AccountID, 0, 100, *f.now)
			if fence == "trash" {
				if err != nil || len(page.Corrections) != 1 {
					t.Fatal("trashed choices not retained for owner", page, err)
				}
			} else if err == nil {
				t.Fatal("feed passed account fence")
			}
		})
	}
}

func TestMySQLContentCorrectionHTTPContractStrictBodyAndSessionScope(t *testing.T) {
	f := newSharingFixture(t)
	auth, err := NewMySQLSessions(f.db, f.uploads.now)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewHTTPRouter("albums.example", auth, f.uploads)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base := "https://albums.example/v1/albums/library/versions/" + f.version.VersionID + "/content-correction"
	input := correctionInput(f)
	data, _ := json.Marshal(input)
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, r)
		return out
	}
	for _, entry := range []struct{ method, path, body, schema string }{{"GET", base, "", "ContentCorrection"}, {"PUT", base, string(data), "ContentCorrection"}, {"GET", "https://albums.example/v1/albums/content-corrections?after=0&limit=1", "", "ContentCorrectionPage"}} {
		out := request(entry.method, entry.path, entry.body, f.session.Token)
		if out.Code != 200 || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(out.Code, out.Body.String())
		}
		var value any
		if json.Unmarshal(out.Body.Bytes(), &value) != nil || doc.Components.Schemas[entry.schema].Value.VisitJSON(value) != nil {
			t.Fatal("response does not match shared contract", out.Body.String())
		}
	}
	for _, body := range []string{`null`, `{}`, `[]`, string(data) + `{}`, strings.Replace(string(data), `"expectedRevision":0`, `"expectedRevision":null`, 1), strings.Replace(string(data), `"requestID":`, `"owner":"foreign","requestID":`, 1), strings.Replace(string(data), `"requestID":`, `"requestID":"`+input.RequestID+`","requestID":`, 1), strings.Replace(string(data), `"dogs"`, `"unknown"`, 1), strings.Replace(string(data), `"excluded":["people"]`, `"excluded":["dogs"]`, 1), strings.Replace(string(data), `"included":["dogs","cats"]`, `"included":null`, 1), strings.Repeat(" ", 4097), string([]byte{0xff})} {
		out := request("PUT", base, body, f.session.Token)
		if out.Code != 422 {
			t.Fatal("invalid request accepted", out.Code, body)
		}
	}
	for _, entry := range []struct {
		path   string
		status int
	}{
		{base + "?owner=foreign", 422},
		{"https://albums.example/v1/albums/content-corrections?owner=foreign", 422},
		{"https://albums.example/v1/albums/content-corrections?after=0&after=1", 400},
	} {
		if out := request("GET", entry.path, "", f.session.Token); out.Code != entry.status {
			t.Fatal("query injection", out.Code)
		}
	}
	if out := request("GET", base, "", ""); out.Code != http.StatusUnauthorized {
		t.Fatal("anonymous correction read", out.Code)
	}
	if out := request("GET", strings.Replace(base, "albums.example", "health.example", 1), "", f.session.Token); out.Code != http.StatusMisdirectedRequest {
		t.Fatal("foreign host", out.Code)
	}
	if f.signer.calls.Load() != 0 {
		t.Fatal("category mutation authorized a media download")
	}
}
