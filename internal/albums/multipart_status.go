package albums

import (
	"context"
	"errors"
)

type MultipartStatus struct {
	ResourceID string                  `json:"resourceID"`
	State      string                  `json:"state"`
	Parts      []MultipartUploadedPart `json:"parts"`
}

func (s *MultipartService) Status(ctx context.Context, owner, uploadID, resourceID string) (MultipartStatus, error) {
	if _, _, err := s.resource(ctx, owner, uploadID, resourceID); err != nil {
		return MultipartStatus{}, err
	}
	result := MultipartStatus{resourceID, "pending", []MultipartUploadedPart{}}
	intent, err := s.repository.GetMultipart(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartStatus{}, err
	}
	if err := readyMultipart(intent, uploadID, resourceID); err != nil {
		if errors.Is(err, ErrMultipartPending) {
			return result, nil
		}
		return MultipartStatus{}, err
	}
	completion, err := s.repository.GetMultipartCompletion(ctx, owner, uploadID, resourceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return MultipartStatus{}, err
	}
	if err == nil {
		if completion.UploadID != uploadID || completion.ResourceID != resourceID {
			return MultipartStatus{}, ErrConflict
		}
		switch completion.State {
		case "assembled":
			if !validMultipartVersion(completion.ObjectVersion) {
				return MultipartStatus{}, ErrConflict
			}
			result.State = "assembled"
			return result, nil
		case "unknown", "completing":
			return result, nil
		case "retryable":
		default:
			return MultipartStatus{}, ErrConflict
		}
	}
	parts, err := s.Progress(ctx, owner, uploadID, resourceID)
	if err != nil {
		return MultipartStatus{}, err
	}
	result.State = "uploading"
	if parts != nil {
		result.Parts = parts
	}
	return result, nil
}
