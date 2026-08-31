package media

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
)

const uploadAuthorizationTTL = 10 * time.Minute
const mediaRecordTTL = 24 * time.Hour

var pathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

type UploadRequest struct {
	RequestID string              `json:"requestID"`
	Operation contracts.Operation `json:"operation"`
	MediaID   string              `json:"mediaID"`
	Kind      string              `json:"kind"`
	MIMEType  string              `json:"mimeType"`
	SHA256    string              `json:"sha256"`
	SizeBytes int64               `json:"sizeBytes"`
}

type UploadAuthorization struct {
	ObjectID        string            `json:"objectID"`
	UploadURL       string            `json:"uploadURL"`
	RequiredHeaders map[string]string `json:"requiredHeaders"`
	ExpiresAt       time.Time         `json:"expiresAt"`
}

type Presigner interface {
	PresignPut(context.Context, string, string, string, int64, time.Time) (string, error)
}

type Service struct {
	presigner Presigner
	registry  Registry
	now       func() time.Time
}

func NewService(presigner Presigner, registry Registry, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{presigner: presigner, registry: registry, now: now}
}

func (service *Service) Authorize(
	ctx context.Context,
	principal attestation.Principal,
	request UploadRequest,
) (UploadAuthorization, error) {
	if service == nil || service.presigner == nil || service.registry == nil || principal.KeyID == "" || !pathSegmentPattern.MatchString(principal.DeviceID) ||
		!contracts.ValidRequestID(request.RequestID) || !pathSegmentPattern.MatchString(request.MediaID) {
		return UploadAuthorization{}, fmt.Errorf("%w: invalid media scope", contracts.ErrContractViolation)
	}
	media := contracts.Media{
		ID:        request.MediaID,
		Kind:      request.Kind,
		MIMEType:  request.MIMEType,
		ObjectID:  "pending",
		SHA256:    request.SHA256,
		SizeBytes: request.SizeBytes,
	}
	if err := contracts.ValidateMediaForOperation(request.Operation, media); err != nil {
		return UploadAuthorization{}, err
	}
	objectID := fmt.Sprintf("ai-temp/%s/%s/%s", principal.DeviceID, request.RequestID, request.MediaID)
	now := service.now()
	if err := service.registry.Register(ctx, Record{
		ObjectID:      objectID,
		OwnerKeyID:    principal.KeyID,
		OwnerDeviceID: principal.DeviceID,
		RequestID:     request.RequestID,
		Operation:     request.Operation,
		MediaID:       request.MediaID,
		Kind:          request.Kind,
		MIMEType:      request.MIMEType,
		SHA256:        request.SHA256,
		SizeBytes:     request.SizeBytes,
		ExpiresAt:     now.Add(mediaRecordTTL),
	}); err != nil {
		return UploadAuthorization{}, err
	}
	expiresAt := now.Add(uploadAuthorizationTTL)
	uploadURL, err := service.presigner.PresignPut(
		ctx,
		objectID,
		request.MIMEType,
		request.SHA256,
		request.SizeBytes,
		expiresAt,
	)
	if err != nil {
		return UploadAuthorization{}, err
	}
	return UploadAuthorization{
		ObjectID:  objectID,
		UploadURL: uploadURL,
		RequiredHeaders: map[string]string{
			"Content-Type":         request.MIMEType,
			"Content-Length":       fmt.Sprintf("%d", request.SizeBytes),
			"x-tos-content-sha256": request.SHA256,
		},
		ExpiresAt: expiresAt,
	}, nil
}

func (service *Service) Validate(
	ctx context.Context,
	principal attestation.Principal,
	request contracts.Request,
) error {
	if service == nil || service.registry == nil || principal.KeyID == "" || principal.DeviceID == "" {
		return ErrNotAuthorized
	}
	for _, item := range request.Media {
		record, err := service.registry.Get(ctx, item.ObjectID)
		if errors.Is(err, ErrNotAuthorized) {
			return ErrNotAuthorized
		}
		if err != nil {
			return fmt.Errorf("%w: load media authorization: %v", ErrUnavailable, err)
		}
		if record.DeletedAt != nil || record.ConsumedAt != nil || !service.now().Before(record.ExpiresAt) {
			return ErrNotAuthorized
		}
		expected := Record{
			ObjectID:      item.ObjectID,
			OwnerKeyID:    principal.KeyID,
			OwnerDeviceID: principal.DeviceID,
			RequestID:     request.RequestID,
			Operation:     request.Operation,
			MediaID:       item.ID,
			Kind:          item.Kind,
			MIMEType:      item.MIMEType,
			SHA256:        item.SHA256,
			SizeBytes:     item.SizeBytes,
		}
		if !sameAuthorization(record, expected) {
			return ErrNotAuthorized
		}
	}
	return nil
}

func (service *Service) Consume(
	ctx context.Context,
	principal attestation.Principal,
	request contracts.Request,
	bodyDigest string,
) error {
	_, replay, err := service.Admit(ctx, principal, request, bodyDigest)
	if err != nil {
		return err
	}
	if replay {
		return ErrIdempotencyReplay
	}
	return nil
}

func (service *Service) Admit(
	ctx context.Context,
	principal attestation.Principal,
	request contracts.Request,
	bodyDigest string,
) (AttemptRecord, bool, error) {
	if service == nil || service.registry == nil || principal.KeyID == "" || principal.DeviceID == "" || bodyDigest == "" {
		return AttemptRecord{}, false, ErrNotAuthorized
	}
	expected := make([]Record, 0, len(request.Media))
	for _, item := range request.Media {
		expected = append(expected, Record{
			ObjectID: item.ObjectID, OwnerKeyID: principal.KeyID, OwnerDeviceID: principal.DeviceID,
			RequestID: request.RequestID, Operation: request.Operation, MediaID: item.ID,
			Kind: item.Kind, MIMEType: item.MIMEType, SHA256: item.SHA256, SizeBytes: item.SizeBytes,
		})
	}
	now := service.now().UTC().Truncate(time.Microsecond)
	return service.registry.CommitAttempt(ctx, expected, AttemptRecord{
		RequestID: request.RequestID, OwnerKeyID: principal.KeyID, BodyDigest: bodyDigest,
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}, now)
}

