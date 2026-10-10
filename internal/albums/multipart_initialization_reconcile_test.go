package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInitializationReconcileBindsOnlyDiscoveredSession(t *testing.T) {
	for _, scenario := range []string{"unknown", "initiating", "ready", "missing", "database", "cancel", "expired", "wrong-owner", "submitted"} {
		t.Run(scenario, func(t *testing.T) {
			uploads, repository, _, owner, manifest := uploadFixture(t)
			u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
			if err != nil {
				t.Fatal(err)
			}
			r := &multipartRepositoryStub{uploadTestRepository: repository, intent: MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "unknown"}}
			now := uploads.now()
			switch scenario {
			case "initiating":
				r.intent.State = "initiating"
			case "ready":
				r.intent.State = "ready"
				r.intent.ProviderUploadID = "already-bound"
			case "expired":
				now = u.ExpiresAt
			case "database":
				r.resolveErr = errors.New("database unavailable")
			}
			original := r.intent
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			p := &multipartProviderStub{find: func(_ context.Context, key string) (string, error) {
				calls++
				if key != stagingKey(u, "photo") {
					t.Fatal("wrong discovery key")
				}
				if scenario == "missing" {
					return "", ErrMultipartPending
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "submitted" {
					_, err := repository.Submit(context.Background(), owner, u.ID, now)
					if err != nil {
						t.Fatal(err)
					}
				}
				return "found-session", nil
			}}
			service, _ := NewMultipartService(r, p, func() time.Time { return now })
			caller := owner
			if scenario == "wrong-owner" {
				caller = uuid.NewString()
			}
			result, err := service.ReconcileInitialization(ctx, caller, u.ID, "photo")
			switch scenario {
			case "unknown", "initiating":
				if err != nil || result.ResourceID != "photo" || r.intent.ProviderUploadID != "found-session" || calls != 1 {
					t.Fatal("recovery failed", err)
				}
			case "ready":
				if err != nil || calls != 0 || r.intent != original {
					t.Fatal("ready binding rediscovered", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || r.intent.ProviderUploadID != "found-session" {
					t.Fatal("cancel lost discovered session", err)
				}
			case "submitted":
				if !errors.Is(err, ErrConflict) || r.intent.ProviderUploadID != "found-session" {
					t.Fatal("discovery returned stale parent", err)
				}
			case "expired":
				if !errors.Is(err, ErrExpired) || r.intent.ProviderUploadID != "found-session" {
					t.Fatal("expired discovery reopened upload or lost observation", err)
				}
			default:
				if err == nil || r.intent != original {
					t.Fatal("failed discovery changed binding", err)
				}
			}
			if scenario == "wrong-owner" && calls != 0 {
				t.Fatal("cross account queried COS")
			}
		})
	}
}
