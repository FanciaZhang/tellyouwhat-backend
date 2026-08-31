package media

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
)

func TestAuthorizeScopesObjectToDeviceRequestAndOperationLimits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	service := NewService(fakePresigner{}, NewMemoryRegistry(), func() time.Time { return now })
	principal := attestation.Principal{DeviceID: "device-1", KeyID: "key-1"}
	authorization, err := service.Authorize(context.Background(), principal, UploadRequest{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation: contracts.OperationMealPhotoCapture,
		MediaID:   "meal-photo-1",
		Kind:      "image",
		MIMEType:  "image/jpeg",
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 2 << 20,
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if !strings.Contains(authorization.ObjectID, "device-1/19be2f9e-bd92-4699-b561-e3816092114c/meal-photo-1") {
		t.Fatalf("object is not scoped: %s", authorization.ObjectID)
	}
	if authorization.ExpiresAt.Sub(now) > 15*time.Minute {
		t.Fatalf("upload authorization is too long lived: %s", authorization.ExpiresAt)
	}
}

func TestAuthorizeRejectsOversizedOrUnsupportedMedia(t *testing.T) {
	t.Parallel()

	service := NewService(fakePresigner{}, NewMemoryRegistry(), time.Now)
	principal := attestation.Principal{DeviceID: "device-1", KeyID: "key-1"}
	_, err := service.Authorize(context.Background(), principal, UploadRequest{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c",
		Operation: contracts.OperationMealPhotoCapture,
		MediaID:   "meal-photo-1",
		Kind:      "image",
		MIMEType:  "image/svg+xml",
		SHA256:    strings.Repeat("a", 64),
		SizeBytes: 2 << 20,
	})
	if !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("expected contract violation, got %v", err)
	}
}

func TestAuthorizedMediaMetadataIsImmutableAndRequiredAtSubmission(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	service := NewService(fakePresigner{}, NewMemoryRegistry(), func() time.Time { return now })
	principal := attestation.Principal{DeviceID: "device-1", KeyID: "key-1"}
	upload := UploadRequest{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealPhotoCapture,
		MediaID: "photo-1", Kind: "image", MIMEType: "image/jpeg", SHA256: strings.Repeat("a", 64), SizeBytes: 1024,
	}
	authorization, err := service.Authorize(context.Background(), principal, upload)
	if err != nil {
		t.Fatal(err)
	}
	overwrite := upload
	overwrite.SHA256 = strings.Repeat("b", 64)
	if _, err := service.Authorize(context.Background(), principal, overwrite); !errors.Is(err, ErrAuthorizationConflict) {
		t.Fatalf("expected immutable authorization conflict, got %v", err)
	}
	request := contracts.Request{
		RequestID: upload.RequestID, Operation: upload.Operation,
		Media: []contracts.Media{{ID: upload.MediaID, Kind: upload.Kind, MIMEType: upload.MIMEType, ObjectID: authorization.ObjectID, SHA256: overwrite.SHA256, SizeBytes: upload.SizeBytes}},
	}
	if err := service.Consume(context.Background(), principal, request, "digest-tampered"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("tampered artifact metadata should be rejected, got %v", err)
	}
}

func TestMediaAuthorizationCanBeConsumedOnlyOnce(t *testing.T) {
	t.Parallel()

	service := NewService(fakePresigner{}, NewMemoryRegistry(), time.Now)
	principal := attestation.Principal{DeviceID: "device-1", KeyID: "key-1"}
	upload := UploadRequest{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealPhotoCapture,
		MediaID: "photo-1", Kind: "image", MIMEType: "image/jpeg", SHA256: strings.Repeat("a", 64), SizeBytes: 1024,
	}
	authorization, err := service.Authorize(context.Background(), principal, upload)
	if err != nil {
		t.Fatal(err)
	}
	request := contracts.Request{RequestID: upload.RequestID, Operation: upload.Operation, Media: []contracts.Media{{
		ID: upload.MediaID, Kind: upload.Kind, MIMEType: upload.MIMEType, ObjectID: authorization.ObjectID, SHA256: upload.SHA256, SizeBytes: upload.SizeBytes,
	}}}
	if err := service.Consume(context.Background(), principal, request, "digest"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := service.Consume(context.Background(), principal, request, "digest"); !errors.Is(err, ErrNotAuthorized) && !errors.Is(err, ErrIdempotencyReplay) {
		t.Fatalf("expected single-use or idempotency rejection, got %v", err)
	}
}

func TestMediaRegistryRejectsBatchWithoutPartiallyConsumingIt(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	registry := NewMemoryRegistry()
	first := Record{ObjectID: "object-1", ExpiresAt: now.Add(time.Hour)}
	second := Record{ObjectID: "object-2", ExpiresAt: now.Add(time.Hour)}
	if err := registry.Register(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CommitAttempt(context.Background(), []Record{second}, AttemptRecord{
		RequestID: "request-1", OwnerKeyID: "key-1", BodyDigest: "digest-1", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.CommitAttempt(context.Background(), []Record{first, second}, AttemptRecord{
		RequestID: "request-2", OwnerKeyID: "key-1", BodyDigest: "digest-2", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, now); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("expected atomic batch rejection, got %v", err)
	}
	if _, _, err := registry.CommitAttempt(context.Background(), []Record{first}, AttemptRecord{
		RequestID: "request-3", OwnerKeyID: "key-1", BodyDigest: "digest-3", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, now); err != nil {
		t.Fatalf("first object was partially consumed: %v", err)
	}
}

func TestFailedMediaBatchDoesNotCommitIdempotencyRecord(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	registry := NewMemoryRegistry()
	attempt := AttemptRecord{
		RequestID: "request-1", OwnerKeyID: "key-1", BodyDigest: "digest", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if _, _, err := registry.CommitAttempt(context.Background(), []Record{{ObjectID: "missing"}}, attempt, now); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("expected missing media rejection, got %v", err)
	}
	if _, _, err := registry.CommitAttempt(context.Background(), nil, attempt, now); err != nil {
		t.Fatalf("failed media batch left an idempotency tombstone: %v", err)
	}
}

func TestMediaValidationPreservesRegistryInfrastructureFailure(t *testing.T) {
	t.Parallel()

	service := NewService(fakePresigner{}, failingRegistry{}, time.Now)
	err := service.Validate(context.Background(), attestation.Principal{KeyID: "key-1", DeviceID: "device-1"}, contracts.Request{
		RequestID: "19be2f9e-bd92-4699-b561-e3816092114c", Operation: contracts.OperationMealPhotoCapture,
		Media: []contracts.Media{{ID: "photo-1", ObjectID: "object-1"}},
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected retryable registry error, got %v", err)
	}
}

type fakePresigner struct{}

func (fakePresigner) PresignPut(_ context.Context, objectID, mimeType, digest string, size int64, expiresAt time.Time) (string, error) {
	return "https://tos.example/upload/" + objectID, nil
}

type failingRegistry struct{}

func (failingRegistry) Register(context.Context, Record) error {
	return errors.New("database unavailable")
}
func (failingRegistry) Get(context.Context, string) (Record, error) {
	return Record{}, errors.New("database unavailable")
}
func (failingRegistry) CommitAttempt(context.Context, []Record, AttemptRecord, time.Time) (AttemptRecord, bool, error) {
	return AttemptRecord{}, false, errors.New("database unavailable")
}
