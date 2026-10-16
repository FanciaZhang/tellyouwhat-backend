package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestAlbumContractAndPublicUploadDoNotExposeWorkerProofFields(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _, _, owner, m := uploadFixture(t)
	u, err := s.Create(context.Background(), owner, uuid.NewString(), m)
	if err != nil {
		t.Fatal(err)
	}
	u.LeaseToken = "secret-lease"
	u.Objects = []SealedObject{{Key: "private-key", VersionID: "private-version"}}
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["Upload"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ownerID", "leaseToken", "objects"} {
		if _, ok := value[key]; ok {
			t.Fatalf("worker field exposed: %s", key)
		}
	}
	grants, err := s.Grants(context.Background(), owner, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(grants[0])
	value = nil
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["Grant"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedAlbumPathErrorsUsePublicErrorShape(t *testing.T) {
	s, _, _, owner, _ := uploadFixture(t)
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, s)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://albums.example/v1/albums/uploads/not-a-uuid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d", response.Code)
	}
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 1 || value["code"] != "invalid_upload_request" {
		t.Fatalf("internal parser error leaked: %v", value)
	}
}

func TestCreateAndGetPreserveReconstructionDescriptors(t *testing.T) {
	service, _, _, owner, manifest := uploadFixture(t)
	manifest.Resources[0].Descriptor = &ResourceDescriptor{PhotoKitType: 1, Filename: "原图.heic", UniformTypeIdentifier: "public.heic"}
	router, err := NewHTTPRouter("albums.example", testAccountAuth{identity: AccountIdentity{"albums", owner}}, service)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"requestID": uuid.NewString(), "manifest": manifest})
	created := httptest.NewRecorder()
	router.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "https://albums.example/v1/albums/uploads", bytes.NewReader(payload)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var upload Upload
	if err := json.Unmarshal(created.Body.Bytes(), &upload); err != nil {
		t.Fatal(err)
	}
	fetched := httptest.NewRecorder()
	router.ServeHTTP(fetched, httptest.NewRequest(http.MethodGet, "https://albums.example/v1/albums/uploads/"+upload.ID, nil))
	if fetched.Code != http.StatusOK {
		t.Fatalf("get: %d", fetched.Code)
	}
	var restored Upload
	if err := json.Unmarshal(fetched.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Manifest.Resources[0].Descriptor == nil || *restored.Manifest.Resources[0].Descriptor != *manifest.Resources[0].Descriptor {
		t.Fatal("descriptor lost at API boundary")
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../Contracts/HTTP/AlbumAPI/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(fetched.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["Upload"].Value.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	expected, _ := manifest.Digest()
	if restored.Digest != expected {
		t.Fatal("descriptor not bound to saved digest")
	}
}
