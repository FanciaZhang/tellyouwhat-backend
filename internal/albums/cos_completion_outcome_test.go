package albums

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Real COS can return 409 after Complete while an object version already exists.
// Keep the durable unknown outcome; another API call must not repeat Complete.
func TestCOSCompletionConflictPersistsUnknownWithoutRepeatingProvider(t *testing.T) {
	uploads, repository, _, owner, manifest := uploadFixture(t)
	u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	r := &multipartRepositoryStub{uploadTestRepository: repository, intent: MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "ready", ProviderUploadID: "provider-one"}}
	completed, calls := 0, 0
	provider := cosFixture(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/"+stagingKey(u, "photo") || request.URL.Query().Get("uploadId") != "provider-one" {
			t.Fatal("completion scope changed")
		}
		if request.Method == "GET" {
			return cosResponse(request, fmt.Sprintf(`<ListPartsResult><Key>%s</Key><UploadId>provider-one</UploadId><Part><PartNumber>1</PartNumber><ETag>part</ETag><Size>%d</Size></Part></ListPartsResult>`, stagingKey(u, "photo"), manifest.Resources[0].SizeBytes), ""), nil
		}
		if request.Method != "POST" {
			t.Fatal("unexpected mutation")
		}
		completed++
		response := cosResponse(request, `<Error><Code>OperationAborted</Code><Message>completion conflicts with abort</Message></Error>`, "")
		response.StatusCode = http.StatusConflict
		return response, nil
	})
	service, err := NewMultipartService(r, provider, uploads.now)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := service.Complete(context.Background(), owner, u.ID, "photo"); !errors.Is(err, ErrMultipartPending) {
			t.Fatal("conflicted completion was treated as safe to retry", err)
		}
	}
	if calls != 2 || completed != 1 || r.completion.State != "unknown" || r.completion.ObjectVersion != "" {
		t.Fatal("conflict was repeated or falsely acknowledged", calls, completed, r.completion.State)
	}
	if _, err := service.Part(context.Background(), owner, u.ID, "photo", 1); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown completion allowed another upload grant", err)
	}
}
