package albums

import (
	"context"
	"errors"
	"time"
)

type MultipartAssembly struct {
	ResourceID string `json:"resourceID"`
	State      string `json:"state"`
}

// Complete assembles staging bytes only. It does not submit or verify backup.
func (s *MultipartService) Complete(ctx context.Context, owner, uploadID, resourceID string) (MultipartAssembly, error) {
	u, resource, err := s.resource(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartAssembly{}, err
	}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartAssembly{}, err
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return MultipartAssembly{}, err
	}
	completion, claimed, err := s.repository.ClaimMultipartCompletion(ctx, owner, uploadID, resourceID, s.now().UTC())
	if err != nil {
		return MultipartAssembly{}, err
	}
	if completion.UploadID != uploadID || completion.ResourceID != resourceID || !validOwner(completion.OperationID) {
		return MultipartAssembly{}, ErrConflict
	}
	if claimed {
		if completion.State != "completing" {
			return MultipartAssembly{}, ErrConflict
		}
		version, operationErr := s.provider.CompleteStagingMultipart(ctx, stagingKey(u, resourceID), intent.ProviderUploadID, resource.SizeBytes)
		state := "assembled"
		if operationErr != nil || !validMultipartVersion(version) {
			state = "unknown"
			version = ""
			// COS marks every error after sending Complete with Unknown. Only
			// positively identified preflight/listing failures allow another attempt.
			if !errors.Is(operationErr, ErrCOSMultipartUnknown) && (errors.Is(operationErr, ErrIncompleteBackup) || errors.Is(operationErr, ErrCOSOperation)) {
				state = "retryable"
			}
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		completion, err = s.repository.ResolveMultipartCompletion(persistCtx, owner, uploadID, resourceID, completion.OperationID, state, version, s.now().UTC())
		cancel()
		if err != nil {
			return MultipartAssembly{}, errors.Join(ErrMultipartPending, err)
		}
		if state == "unknown" {
			return MultipartAssembly{}, ErrMultipartPending
		}
		if state == "retryable" {
			return MultipartAssembly{}, operationErr
		}
	}
	if completion.State != "assembled" {
		return MultipartAssembly{}, ErrMultipartPending
	}
	if !validMultipartVersion(completion.ObjectVersion) {
		return MultipartAssembly{}, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return MultipartAssembly{}, err
	}
	return MultipartAssembly{resourceID, "assembled"}, nil
}
