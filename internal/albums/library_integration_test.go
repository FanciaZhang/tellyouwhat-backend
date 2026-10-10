package albums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/getkin/kin-openapi/openapi3"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func checkMySQLAssetVersions(t *testing.T, ctx context.Context, r *MySQLUploads, owner string, uploads []Upload, now time.Time) {
	t.Helper()
	checkMySQLLibraryHTTP(t, r, owner, uploads[0].ID)
	checkMySQLLibraryPageByteBudget(t, ctx, r)
	if err := r.CheckLibrarySchema(ctx); err != nil {
		t.Fatal(err)
	}
	// Concurrent completion replays cannot publish duplicate cursor events.
	var workers sync.WaitGroup
	failures := make(chan error, 20)
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := r.Complete(ctx, owner, uploads[0].ID, "", nil, now)
			failures <- err
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var after int64
	seen := map[string]bool{}
	for pageIndex := range 3 {
		page, err := r.AssetVersions(ctx, owner, after, 1)
		if err != nil || len(page.Versions) != 1 || page.NextSequence <= after || page.HasMore != (pageIndex < 2) {
			t.Fatalf("page %+v %v", page, err)
		}
		v := page.Versions[0]
		if seen[v.VersionID] {
			t.Fatal("duplicate version in cursor stream")
		}
		seen[v.VersionID] = true
		if len(v.Objects) != 1 || v.Manifest.Metadata == nil || v.Manifest.Resources[0].Descriptor == nil {
			t.Fatal("restore evidence lost")
		}
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"sealed_objects", "albums/originals/", "immutable-v1"} {
			if strings.Contains(string(data), secret) {
				t.Fatal("provider proof exposed")
			}
		}
		fetched, err := r.AssetVersion(ctx, owner, v.VersionID)
		if err != nil || fetched.ManifestDigest != v.ManifestDigest || fetched.Sequence != v.Sequence {
			t.Fatal("version fetch mismatch")
		}
		if _, err := r.AssetVersion(ctx, uuid.NewString(), v.VersionID); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-account read")
		}
		after = page.NextSequence
	}
	page, err := r.AssetVersions(ctx, owner, after, 10)
	if err != nil || page.HasMore || len(page.Versions) != 0 || page.NextSequence != after {
		t.Fatal("end cursor is unstable")
	}
	other, err := r.AssetVersions(ctx, uuid.NewString(), 0, 10)
	if err != nil || len(other.Versions) != 0 {
		t.Fatal("cross-account listing")
	}
	for _, args := range [][2]int64{{-1, 1}, {0, 0}, {0, 101}} {
		if _, err := r.AssetVersions(ctx, owner, args[0], int(args[1])); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal("invalid cursor accepted")
		}
	}
}

func checkMySQLLibraryHTTP(t *testing.T, r *MySQLUploads, owner, versionID string) {
	t.Helper()
	service, _, _, _, _ := uploadFixture(t)
	service.repository = r
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, service)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ path, schema string }{
		{"/v1/albums/library/versions?after=0&limit=1", "AssetVersionPage"},
		{"/v1/albums/library/versions/" + versionID, "AssetVersion"},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "https://albums.example"+entry.path, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(response.Code, response.Body.String())
		}
		var value any
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[entry.schema].Value.VisitJSON(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"?owner=other", "?after=-1", "?limit=101", "?limit=0", "?after=0&after=1", "?limit=", "/" + versionID + "?owner=other"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "https://albums.example/v1/albums/library/versions"+path, nil))
		if response.Code < 400 || response.Code >= 500 {
			t.Fatal("invalid query", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "https://albums.example/v1/albums/library/versions", strings.NewReader("{}")))
	if response.Code != 422 {
		t.Fatal("accepted body", response.Code)
	}
	foreign, _ := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", uuid.NewString()}}, service)
	response = httptest.NewRecorder()
	foreign.ServeHTTP(response, httptest.NewRequest("GET", "https://albums.example/v1/albums/library/versions/"+versionID, nil))
	if response.Code != 404 || !strings.Contains(response.Body.String(), "asset_version_not_found") {
		t.Fatal("cross account leak")
	}
}

func checkMySQLLibraryPageByteBudget(t *testing.T, ctx context.Context, r *MySQLUploads) {
	t.Helper()
	owner := uuid.NewString()
	defer func() {
		r.db.ExecContext(context.Background(), `DELETE FROM album_asset_versions WHERE owner_id=?`, owner)
		r.db.ExecContext(context.Background(), `DELETE FROM album_uploads WHERE owner_id=?`, owner)
		r.db.ExecContext(context.Background(), `DELETE FROM album_storage_accounts WHERE owner_id=?`, owner)
	}()
	if err := r.ProvisionAccount(ctx, owner, 4096); err != nil {
		t.Fatal(err)
	}
	service, _, _, _, _ := uploadFixture(t)
	service.repository = r
	now := service.now()
	for range 2 {
		manifest := Manifest{AssetID: uuid.NewString(), SourceRevision: "large-metadata", Kind: Photo}
		for index := range 32 {
			role, kind := Alternate, 100
			if index == 0 {
				role, kind = OriginalPhoto, 1
			}
			manifest.Resources = append(manifest.Resources, Resource{ID: fmt.Sprintf("r%d", index), Role: role, SizeBytes: 1, SHA256: strings.Repeat("a", 64), Descriptor: &ResourceDescriptor{PhotoKitType: kind, Filename: strings.Repeat("\x01", 4096), UniformTypeIdentifier: strings.Repeat("\x01", 512)}})
		}
		upload, err := service.Create(ctx, owner, uuid.NewString(), manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.Submit(ctx, owner, upload.ID, now); err != nil {
			t.Fatal(err)
		}
		token := uuid.NewString()
		if _, err = r.Claim(ctx, owner, upload.ID, token, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		var proof []SealedObject
		for _, resource := range manifest.Resources {
			proof = append(proof, SealedObject{ResourceID: resource.ID, Key: sealedKey(upload, resource.ID), VersionID: "v1", SizeBytes: resource.SizeBytes, SHA256: resource.SHA256})
		}
		if _, err = r.Complete(ctx, owner, upload.ID, token, proof, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := r.AssetVersions(ctx, owner, 0, 100)
	if err != nil || len(first.Versions) != 1 || !first.HasMore {
		t.Fatal("byte budget not enforced", err, len(first.Versions))
	}
	encoded, err := json.Marshal(first)
	if err != nil || len(encoded) > 1<<20 {
		t.Fatal("oversized response")
	}
	second, err := r.AssetVersions(ctx, owner, first.NextSequence, 100)
	if err != nil || len(second.Versions) != 1 || second.HasMore || second.Versions[0].VersionID == first.Versions[0].VersionID {
		t.Fatal("truncated page cursor lost version", err)
	}
}
