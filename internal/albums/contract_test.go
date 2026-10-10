package albums

import (
	"context"
	"encoding/json"
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
