package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type multipartRepositoryStub struct {
	*uploadTestRepository
	completion           MultipartCompletion
	completionErr        error
	intent               MultipartIntent
	beginErr, resolveErr error
	resolvedAfterCancel  bool
}

func (r *multipartRepositoryStub) BeginMultipart(_ context.Context, owner, id, resource string, _ time.Time) (MultipartIntent, bool, error) {
	if r.beginErr != nil {
		return MultipartIntent{}, false, r.beginErr
	}
	if r.intent.IntentID != "" {
		return r.intent, false, nil
	}
	r.intent = MultipartIntent{UploadID: id, ResourceID: resource, IntentID: uuid.NewString(), State: "initiating"}
	return r.intent, true, nil
}
func (r *multipartRepositoryStub) ResolveMultipart(ctx context.Context, owner, id, resource, intent, provider string, _ time.Time) (MultipartIntent, error) {
	if ctx.Err() != nil {
		return MultipartIntent{}, ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return MultipartIntent{}, errors.New("unbounded persistence")
	}
	r.resolvedAfterCancel = true
	if r.resolveErr != nil {
		return MultipartIntent{}, r.resolveErr
	}
	r.intent.ProviderUploadID = provider
	r.intent.State = "unknown"
	if provider != "" {
		r.intent.State = "ready"
	}
	return r.intent, nil
}
func (r *multipartRepositoryStub) GetMultipart(_ context.Context, owner, id, resource string) (MultipartIntent, error) {
	if r.intent.IntentID == "" {
		return MultipartIntent{}, ErrNotFound
	}
	return r.intent, nil
}

type multipartProviderStub struct {
	complete func(context.Context, string, string, int64) (string, error)
	init     func(context.Context, string) (string, error)
	part     func(context.Context, string, string, Resource, int, time.Time) (MultipartPartGrant, error)
	list     func(context.Context, string, string, int64) ([]MultipartUploadedPart, error)
}

func (p *multipartProviderStub) InitiateStagingMultipart(ctx context.Context, key string) (string, error) {
	return p.init(ctx, key)
}
func (p *multipartProviderStub) AuthorizePart(ctx context.Context, key, id string, r Resource, n int, expiry time.Time) (MultipartPartGrant, error) {
	return p.part(ctx, key, id, r, n, expiry)
}
func (p *multipartProviderStub) ListStagingMultipartParts(ctx context.Context, key, id string, size int64) ([]MultipartUploadedPart, error) {
	return p.list(ctx, key, id, size)
}

func TestMultipartServiceInitializationPersistsBeforeProviderAndReusesBinding(t *testing.T) {
	for _, scenario := range []string{"success", "unknown", "database-before", "database-after", "cancel", "expiry"} {
		t.Run(scenario, func(t *testing.T) {
			uploads, repository, _, owner, manifest := uploadFixture(t)
			u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
			if err != nil {
				t.Fatal(err)
			}
			now := uploads.now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &multipartRepositoryStub{uploadTestRepository: repository}
			if scenario == "database-before" {
				r.beginErr = errors.New("db unavailable")
			}
			if scenario == "database-after" {
				r.resolveErr = errors.New("db unavailable")
			}
			calls := 0
			p := &multipartProviderStub{init: func(_ context.Context, key string) (string, error) {
				calls++
				if r.intent.State != "initiating" || key != stagingKey(u, "photo") {
					t.Fatal("provider before durable intent or wrong key")
				}
				switch scenario {
				case "unknown":
					return "", ErrCOSMultipartUnknown
				case "cancel":
					cancel()
				case "expiry":
					now = u.ExpiresAt
				}
				return "provider-one", nil
			}}
			service, _ := NewMultipartService(r, p, func() time.Time { return now })
			result, err := service.Initialize(ctx, owner, u.ID, "photo")
			if scenario == "success" {
				if err != nil || result.ResourceID != "photo" || result.SizeBytes != manifest.Resources[0].SizeBytes || result.PartCount != 1 {
					t.Fatal("initialization failed", err)
				}
			} else if err == nil {
				t.Fatal("failure returned usable session")
			}
			if scenario == "database-before" {
				if calls != 0 {
					t.Fatal("contacted provider before commit")
				}
				return
			}
			if scenario == "cancel" && (!r.resolvedAfterCancel || r.intent.ProviderUploadID != "provider-one") {
				t.Fatal("client cancellation lost provider ID")
			}
			if scenario == "unknown" && r.intent.State != "unknown" {
				t.Fatal("unknown outcome discarded")
			}
			_, replayErr := service.Initialize(context.Background(), owner, u.ID, "photo")
			if calls != 1 {
				t.Fatal("retry created another provider session")
			}
			if scenario == "success" && replayErr != nil {
				t.Fatal("ready session not replayable", replayErr)
			}
			if (scenario == "unknown" || scenario == "database-after") && !errors.Is(replayErr, ErrMultipartPending) {
				t.Fatal("uncertain session not held for reconciliation", replayErr)
			}
		})
	}
}

func TestMultipartServicePartsUseStoredOwnershipAndManifest(t *testing.T) {
	uploads, repository, _, owner, manifest := uploadFixture(t)
	u, err := uploads.Create(context.Background(), owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := uploads.now()
	r := &multipartRepositoryStub{uploadTestRepository: repository, intent: MultipartIntent{UploadID: u.ID, ResourceID: "photo", IntentID: uuid.NewString(), State: "ready", ProviderUploadID: "stored-id"}}
	parts, lists := 0, 0
	p := &multipartProviderStub{
		part: func(_ context.Context, key, id string, resource Resource, number int, expiry time.Time) (MultipartPartGrant, error) {
			parts++
			if key != stagingKey(u, "photo") || id != "stored-id" || resource != manifest.Resources[0] || number != 1 || !expiry.Equal(now.Add(10*time.Minute)) {
				t.Fatal("wrong signed scope")
			}
			return MultipartPartGrant{PartNumber: number}, nil
		},
		list: func(_ context.Context, key, id string, size int64) ([]MultipartUploadedPart, error) {
			lists++
			if key != stagingKey(u, "photo") || id != "stored-id" || size != manifest.Resources[0].SizeBytes {
				t.Fatal("wrong progress scope")
			}
			return []MultipartUploadedPart{{1, size}}, nil
		},
	}
	service, _ := NewMultipartService(r, p, func() time.Time { return now })
	if _, err := service.Part(context.Background(), owner, u.ID, "photo", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Progress(context.Background(), owner, u.ID, "photo"); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{uuid.NewString(), "bad-owner"} {
		if _, err := service.Part(context.Background(), other, u.ID, "photo", 1); err == nil {
			t.Fatal("cross account part")
		}
		if _, err := service.Progress(context.Background(), other, u.ID, "photo"); err == nil {
			t.Fatal("cross account progress")
		}
	}
	if _, err := service.Part(context.Background(), owner, u.ID, "photo", 2); err == nil {
		t.Fatal("extra part")
	}
	r.intent.State = "unknown"
	if _, err := service.Part(context.Background(), owner, u.ID, "photo", 1); !errors.Is(err, ErrMultipartPending) {
		t.Fatal("unknown grant", err)
	}
	r.intent.State = "ready"
	r.intent.ResourceID = "other"
	if _, err := service.Progress(context.Background(), owner, u.ID, "photo"); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong resource binding", err)
	}
	now = u.ExpiresAt
	if _, err := service.Part(context.Background(), owner, u.ID, "photo", 1); !errors.Is(err, ErrExpired) {
		t.Fatal("expired grant", err)
	}
	if parts != 1 || lists != 1 {
		t.Fatal("invalid request reached provider")
	}
}

func (r *multipartRepositoryStub) ClaimMultipartCompletion(_ context.Context, owner, id, resource string, _ time.Time) (MultipartCompletion, bool, error) {
	if r.completion.OperationID != "" && r.completion.State != "retryable" {
		return r.completion, false, nil
	}
	r.completion = MultipartCompletion{UploadID: id, ResourceID: resource, OperationID: uuid.NewString(), State: "completing"}
	return r.completion, true, nil
}
func (r *multipartRepositoryStub) ResolveMultipartCompletion(ctx context.Context, owner, id, resource, operation, state, version string, _ time.Time) (MultipartCompletion, error) {
	if ctx.Err() != nil {
		return MultipartCompletion{}, ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return MultipartCompletion{}, errors.New("unbounded persistence")
	}
	if r.completionErr != nil {
		return MultipartCompletion{}, r.completionErr
	}
	r.completion.State, r.completion.ObjectVersion = state, version
	return r.completion, nil
}
func (r *multipartRepositoryStub) GetMultipartCompletion(_ context.Context, owner, id, resource string) (MultipartCompletion, error) {
	if r.completion.OperationID == "" {
		return MultipartCompletion{}, ErrNotFound
	}
	return r.completion, nil
}
func (p *multipartProviderStub) CompleteStagingMultipart(ctx context.Context, key, id string, size int64) (string, error) {
	return p.complete(ctx, key, id, size)
}
