package albums

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestMultipartCompletionPersistsOutcomeAndNeverBlindlyRepeats(t *testing.T) {
	for _, scenario := range []string{"success", "preflight", "unknown", "generic-error", "missing-version", "database", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			uploads, repository, _, owner, manifest := uploadFixture(t)
			u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
			if err != nil {
				t.Fatal(err)
			}
			r := &multipartRepositoryStub{uploadTestRepository: repository, intent: MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "ready", ProviderUploadID: "provider-one"}}
			if scenario == "database" {
				r.completionErr = errors.New("db lost")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			p := &multipartProviderStub{complete: func(_ context.Context, key, id string, size int64) (string, error) {
				calls++
				if r.completion.State != "completing" || key != stagingKey(u, "photo") || id != "provider-one" || size != manifest.Resources[0].SizeBytes {
					t.Fatal("wrong completion scope or missing durable claim")
				}
				if calls == 1 {
					switch scenario {
					case "preflight":
						return "", ErrIncompleteBackup
					case "unknown":
						return "", errors.Join(ErrCOSOperation, ErrCOSMultipartUnknown)
					case "generic-error":
						return "", errors.New("unexpected provider failure")
					case "missing-version":
						return "", nil
					case "cancel":
						cancel()
					}
				}
				return "assembled-version", nil
			}}
			service, _ := NewMultipartService(r, p, uploads.now)
			result, err := service.Complete(ctx, owner, u.ID, "photo")
			if scenario == "success" {
				if err != nil || result.State != "assembled" {
					t.Fatal("assembly failed", err)
				}
			} else if err == nil {
				t.Fatal("failure returned success")
			}
			if scenario == "cancel" && r.completion.ObjectVersion != "assembled-version" {
				t.Fatal("cancellation lost version")
			}
			if scenario != "preflight" {
				if _, err := service.Part(context.Background(), owner, u.ID, "photo", 1); !errors.Is(err, ErrConflict) {
					t.Fatal("issued grant during or after completion", err)
				}
			}
			replay, replayErr := service.Complete(context.Background(), owner, u.ID, "photo")
			switch scenario {
			case "success", "cancel":
				if calls != 1 || replayErr != nil || replay.State != "assembled" {
					t.Fatal("assembled replay repeated provider", replayErr)
				}
			case "preflight":
				if calls != 2 || replayErr != nil {
					t.Fatal("known preflight failure did not retry", replayErr)
				}
			default:
				if calls != 1 || !errors.Is(replayErr, ErrMultipartPending) {
					t.Fatal("unknown outcome repeated provider", replayErr)
				}
			}
		})
	}
}
