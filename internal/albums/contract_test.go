package albums

import (
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
