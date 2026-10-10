package albums

import (
	"context"
	"errors"
	"time"
)

// ReconcileCompletion is a trusted worker operation, not a client proof API.
// It also records late success for expired parents, without renewing quota or
// granting more writes. Failed observations leave the uncertain record intact.
func (s *MultipartService) ReconcileCompletion(ctx context.Context, owner, uploadID, resourceID string) (MultipartAssembly, error) {
	if !validOwner(owner) {
		return MultipartAssembly{}, ErrOwner
	}
	if !validOwner(uploadID) || !resourceIDPattern.MatchString(resourceID) {
		return MultipartAssembly{}, ErrNotFound
	}
	u, err := s.repository.Get(ctx, owner, uploadID)
	if err != nil {
		return MultipartAssembly{}, err
	}
	if u.OwnerID != owner || u.ID != uploadID {
		return MultipartAssembly{}, ErrNotFound
	}
	var resource Resource
	for _, candidate := range u.Manifest.Resources {
		if candidate.ID == resourceID {
			resource = candidate
			break
		}
	}
	if resource.ID == "" {
		return MultipartAssembly{}, ErrNotFound
	}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartAssembly{}, err
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		return MultipartAssembly{}, err
	}
	record, err := s.repository.GetMultipartCompletion(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartAssembly{}, err
	}
	if record.UploadID != uploadID || record.ResourceID != resourceID || !validOwner(record.OperationID) {
		return MultipartAssembly{}, ErrConflict
	}
	if record.State == "assembled" && validMultipartVersion(record.ObjectVersion) {
		return MultipartAssembly{resourceID, "assembled"}, nil
	}
	if record.State != "unknown" && record.State != "completing" {
		return MultipartAssembly{}, ErrConflict
	}
	version, err := s.provider.VerifyStagingResource(ctx, stagingKey(u, resourceID), resource)
	if err != nil {
		return MultipartAssembly{}, errors.Join(ErrMultipartPending, err)
	}
	if !validMultipartVersion(version) {
		return MultipartAssembly{}, ErrMultipartPending
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err = s.repository.ResolveMultipartCompletion(persistCtx, owner, uploadID, resourceID, record.OperationID, "assembled", version, s.now().UTC())
	if err != nil {
		return MultipartAssembly{}, errors.Join(ErrMultipartPending, err)
	}
	return MultipartAssembly{resourceID, "assembled"}, nil
}
