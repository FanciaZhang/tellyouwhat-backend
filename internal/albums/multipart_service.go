package albums

import (
	"context"
	"errors"
	"time"
)

var ErrMultipartPending = errors.New("album multipart initialization requires reconciliation")

type MultipartRepository interface {
	ClaimMultipartCompletion(context.Context, string, string, string, time.Time) (MultipartCompletion, bool, error)
	ResolveMultipartCompletion(context.Context, string, string, string, string, string, string, time.Time) (MultipartCompletion, error)
	GetMultipartCompletion(context.Context, string, string, string) (MultipartCompletion, error)
	Get(context.Context, string, string) (Upload, error)
	BeginMultipart(context.Context, string, string, string, time.Time) (MultipartIntent, bool, error)
	ResolveMultipart(context.Context, string, string, string, string, string, time.Time) (MultipartIntent, error)
	GetMultipart(context.Context, string, string, string) (MultipartIntent, error)
}

type MultipartProvider interface {
	VerifyStagingResource(context.Context, string, Resource) (string, error)
	CompleteStagingMultipart(context.Context, string, string, int64) (string, error)
	InitiateStagingMultipart(context.Context, string) (string, error)
	AuthorizePart(context.Context, string, string, Resource, int, time.Time) (MultipartPartGrant, error)
	ListStagingMultipartParts(context.Context, string, string, int64) ([]MultipartUploadedPart, error)
}

type MultipartSession struct {
	ResourceID string `json:"resourceID"`
	SizeBytes  int64  `json:"sizeBytes"`
	PartSize   int64  `json:"partSize"`
	PartCount  int    `json:"partCount"`
}

type MultipartService struct {
	repository MultipartRepository
	provider   MultipartProvider
	now        func() time.Time
}

func NewMultipartService(repository MultipartRepository, provider MultipartProvider, now func() time.Time) (*MultipartService, error) {
	if repository == nil || provider == nil {
		return nil, errors.New("invalid multipart configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &MultipartService{repository, provider, now}, nil
}

func (s *MultipartService) resource(ctx context.Context, owner, uploadID, resourceID string) (Upload, Resource, error) {
	if err := ctx.Err(); err != nil {
		return Upload{}, Resource{}, err
	}
	if !validOwner(owner) {
		return Upload{}, Resource{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return Upload{}, Resource{}, ErrNotFound
	}
	u, err := s.repository.Get(ctx, owner, uploadID)
	if err != nil {
		return Upload{}, Resource{}, err
	}
	if u.OwnerID != owner || u.ID != uploadID {
		return Upload{}, Resource{}, ErrNotFound
	}
	if err := checkMultipartParent(u, resourceID, s.now()); err != nil {
		return Upload{}, Resource{}, err
	}
	for _, resource := range u.Manifest.Resources {
		if resource.ID == resourceID {
			return u, resource, nil
		}
	}
	return Upload{}, Resource{}, ErrNotFound
}

func readyMultipart(intent MultipartIntent, uploadID, resourceID string) error {
	if intent.UploadID != uploadID || intent.ResourceID != resourceID || !validOwner(intent.IntentID) {
		return ErrConflict
	}
	if intent.State != "ready" {
		return ErrMultipartPending
	}
	if !validCOSMultipartID(intent.ProviderUploadID) {
		return ErrConflict
	}
	return nil
}

// Initialize is the only orchestration path allowed to create a provider upload.
// Client retries reuse the durable binding; unknown outcomes never reinitialize.
func (s *MultipartService) Initialize(ctx context.Context, owner, uploadID, resourceID string) (MultipartSession, error) {
	u, resource, err := s.resource(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartSession{}, err
	}
	layout, err := NewMultipartLayout(resource.SizeBytes)
	if err != nil {
		return MultipartSession{}, err
	}
	intent, created, err := s.repository.BeginMultipart(ctx, owner, uploadID, resourceID, s.now().UTC())
	if err != nil {
		return MultipartSession{}, err
	}
	if created {
		if intent.UploadID != uploadID || intent.ResourceID != resourceID || !validOwner(intent.IntentID) || intent.State != "initiating" || intent.ProviderUploadID != "" {
			return MultipartSession{}, ErrConflict
		}
		providerID, operationErr := s.provider.InitiateStagingMultipart(ctx, stagingKey(u, resourceID))
		if operationErr != nil || !validCOSMultipartID(providerID) {
			providerID = ""
		}
		// A disconnected client must not discard the only provider ID we observed.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		intent, err = s.repository.ResolveMultipart(persistCtx, owner, uploadID, resourceID, intent.IntentID, providerID, s.now().UTC())
		cancel()
		if err != nil {
			return MultipartSession{}, errors.Join(ErrMultipartPending, err)
		}
		if operationErr != nil || providerID == "" {
			return MultipartSession{}, ErrMultipartPending
		}
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return MultipartSession{}, err
	}
	// Initialization may outlive the parent validity or client connection.
	if _, _, err := s.resource(ctx, owner, uploadID, resourceID); err != nil {
		return MultipartSession{}, err
	}
	return MultipartSession{resourceID, layout.SizeBytes, layout.PartSize, layout.PartCount}, nil
}

func (s *MultipartService) Part(ctx context.Context, owner, uploadID, resourceID string, number int) (MultipartPartGrant, error) {
	u, resource, err := s.resource(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	layout, err := NewMultipartLayout(resource.SizeBytes)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	if _, _, err := layout.Part(number); err != nil {
		return MultipartPartGrant{}, err
	}
	completion, completionErr := s.repository.GetMultipartCompletion(ctx, owner, uploadID, resourceID)
	if completionErr != nil && !errors.Is(completionErr, ErrNotFound) {
		return MultipartPartGrant{}, completionErr
	}
	if completionErr == nil && completion.State != "retryable" {
		return MultipartPartGrant{}, ErrConflict
	}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return MultipartPartGrant{}, err
	}
	expiry := minTime(u.ExpiresAt, s.now().Add(10*time.Minute))
	return s.provider.AuthorizePart(ctx, stagingKey(u, resourceID), intent.ProviderUploadID, resource, number, expiry)
}

func (s *MultipartService) Progress(ctx context.Context, owner, uploadID, resourceID string) ([]MultipartUploadedPart, error) {
	u, resource, err := s.resource(ctx, owner, uploadID, resourceID)
	if err != nil {
		return nil, err
	}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return nil, err
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return nil, err
	}
	return s.provider.ListStagingMultipartParts(ctx, stagingKey(u, resourceID), intent.ProviderUploadID, resource.SizeBytes)
}

var _ MultipartRepository = (*MySQLUploads)(nil)
var _ MultipartProvider = (*COSObjects)(nil)
