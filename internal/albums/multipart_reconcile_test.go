package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMultipartReconciliationOnlyCommitsVerifiedVersion(t *testing.T) {
	for _, scenario := range []string{"unknown", "completing", "expired", "mismatch", "database", "already-assembled", "retryable", "wrong-owner", "stale-operation"} {
		t.Run(scenario, func(t *testing.T) {
			uploads, repository, _, owner, manifest := uploadFixture(t)
			u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
			if err != nil {
				t.Fatal(err)
			}
			r := &multipartRepositoryStub{uploadTestRepository: repository, intent: MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "ready", ProviderUploadID: "provider-one"}, completion: MultipartCompletion{UploadID: u.ID, ResourceID: "photo", OperationID: uuid.NewString(), State: "unknown"}}
			now := uploads.now()
			switch scenario {
			case "completing":
				r.completion.State = "completing"
			case "expired":
				now = u.ExpiresAt.Add(time.Hour)
			case "database":
				r.completionErr = errors.New("database lost")
			case "already-assembled":
				r.completion.State = "assembled"
				r.completion.ObjectVersion = "previous-version"
			case "retryable":
				r.completion.State = "retryable"
			}
			original := r.completion
			calls := 0
			p := &multipartProviderStub{verify: func(_ context.Context, key string, resource Resource) (string, error) {
				calls++
				if key != stagingKey(u, "photo") || resource != manifest.Resources[0] {
					t.Fatal("wrong resource verified")
				}
				if scenario == "mismatch" {
					return "", ErrIncompleteBackup
				}
				return "checked-version", nil
			}}
			service, _ := NewMultipartService(r, p, func() time.Time { return now })
			caller := owner
			if scenario == "wrong-owner" {
				caller = uuid.NewString()
			}
			var result MultipartAssembly
			if scenario == "stale-operation" {
				result, err = service.reconcileCompletion(context.Background(), caller, u.ID, "photo", uuid.NewString())
			} else {
				result, err = service.ReconcileCompletion(context.Background(), caller, u.ID, "photo")
			}
			switch scenario {
			case "unknown", "completing", "expired":
				if err != nil || result.State != "assembled" || r.completion.ObjectVersion != "checked-version" || calls != 1 {
					t.Fatal("checked recovery failed", err)
				}
				if repository.values[u.ID].State != Uploading {
					t.Fatal("reconciliation declared backup verified")
				}
			case "already-assembled":
				if err != nil || calls != 0 || r.completion != original {
					t.Fatal("assembled result not replayed", err)
				}
			default:
				if err == nil || r.completion != original {
					t.Fatal("failed observation changed durable state", err)
				}
				if (scenario == "retryable" || scenario == "wrong-owner" || scenario == "stale-operation") && calls != 0 {
					t.Fatal("unauthorized reconciliation read bytes")
				}
			}
		})
	}
}
