package albums

import (
	"context"
	"errors"
	"time"
)

// ReconcileInitialization is trusted control work; it never creates or aborts a
// provider session. The unique staging key is derived from the owned manifest.
func (s *MultipartService) ReconcileInitialization(ctx context.Context, owner, uploadID, resourceID string) (MultipartSession, error) {
	return s.reconcileInitialization(ctx, owner, uploadID, resourceID, "")
}

func (s *MultipartService) reconcileInitialization(ctx context.Context, owner, uploadID, resourceID, intentID string) (MultipartSession, error) {
	if !validOwner(owner) {
		return MultipartSession{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartSession{}, ErrNotFound
	}
	u, err := s.repository.Get(ctx, owner, uploadID)
	if err != nil {
		return MultipartSession{}, err
	}
	if u.OwnerID != owner || u.ID != uploadID {
		return MultipartSession{}, ErrNotFound
	}
	var resource Resource
	for _, candidate := range u.Manifest.Resources {
		if candidate.ID == resourceID {
			resource = candidate
			break
		}
	}
	if resource.ID == "" {
		return MultipartSession{}, ErrNotFound
	}
	layout, err := NewMultipartLayout(resource.SizeBytes)
	if err != nil {
		return MultipartSession{}, err
	}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartSession{}, err
	}
	if intent.UploadID != uploadID || intent.ResourceID != resourceID || !validOwner(intent.IntentID) {
		return MultipartSession{}, ErrConflict
	}
	if intentID != "" && intent.IntentID != intentID {
		return MultipartSession{}, ErrLease
	}
	if intent.State != "ready" {
		if intent.State != "initiating" && intent.State != "unknown" {
			return MultipartSession{}, ErrConflict
		}
		id, err := s.provider.FindStagingMultipart(ctx, stagingKey(u, resourceID))
		if err != nil {
			return MultipartSession{}, errors.Join(ErrMultipartPending, err)
		}
		if !validCOSMultipartID(id) {
			return MultipartSession{}, ErrMultipartPending
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		intent, err = s.repository.ResolveMultipart(persistCtx, owner, uploadID, resourceID, intent.IntentID, id, s.now().UTC())
		cancel()
		if err != nil {
			return MultipartSession{}, errors.Join(ErrMultipartPending, err)
		}
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return MultipartSession{}, err
	}
	// A worker may record late observations; interactive callers still cannot
	// reopen expired work. Neither path issues new upload grants.
	if intentID == "" {
		if _, _, err := s.resource(ctx, owner, uploadID, resourceID); err != nil {
			return MultipartSession{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return MultipartSession{}, err
	}
	return MultipartSession{resourceID, layout.SizeBytes, layout.PartSize, layout.PartCount}, nil
}
