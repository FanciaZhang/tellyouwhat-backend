package albums

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type PhotoWriteRepository interface {
	PhotoWrite(context.Context, string, string, PhotoPreset) (PhotoWriteRecord, error)
	BeginPhotoWrite(context.Context, string, string, PhotoWriteSpec, time.Time) (PhotoWriteRecord, bool, error)
	CompletePhotoWrite(context.Context, PhotoWriteRecord, SealedObject, time.Time) (PhotoWriteRecord, error)
}
type PhotoCandidateStorage interface {
	ReconcilePhotoCandidate(context.Context, PhotoWriteSpec) (SealedObject, error)
	StartPhotoMultipart(context.Context, PhotoWriteSpec) (string, error)
	FindPhotoMultipart(context.Context, PhotoWriteSpec) (string, error)
	StorePhotoMultipartOnce(context.Context, PhotoWriteSpec, string, PhotoCandidate, func(context.Context) error) (SealedObject, error)
	ReconcilePhotoMultipart(context.Context, PhotoWriteSpec, string, func(context.Context) error) (SealedObject, error)
}

// GeneratePhotoDerivative recovers the existing session first. Missing parts
// may regenerate only the exact recorded candidate, without creating a session.
// Returned storage evidence is not authorization
// to delete an original; media quality and system import are separate gates.
func (s *UploadService) GeneratePhotoDerivative(ctx context.Context, owner, versionID string, preset PhotoPreset) (PhotoWriteRecord, error) {
	if rule, _, _, _ := photoPresetRule(preset); rule == "" {
		return PhotoWriteRecord{}, ErrInvalidManifest
	}
	repository, ok := s.repository.(PhotoWriteRepository)
	storage, supported := s.objects.(PhotoCandidateStorage)
	if !ok || !supported {
		return PhotoWriteRecord{}, ErrPhotoCandidate
	}
	version, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if !photoPreviewEligible(version.Manifest, preset) {
		return PhotoWriteRecord{}, ErrPhotoCandidate
	}
	record, err := repository.PhotoWrite(ctx, owner, versionID, preset)
	fresh := false
	var candidate PhotoCandidate
	if errors.Is(err, ErrNotFound) {
		candidate, err = s.PhotoCandidate(ctx, owner, versionID, preset)
		if err != nil {
			return PhotoWriteRecord{}, err
		}
		spec := PhotoWriteSpec{OwnerID: owner, AttemptID: uuid.NewString(), Preset: preset, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height, Multipart: true}
		record, fresh, err = repository.BeginPhotoWrite(ctx, versionID, version.ManifestDigest, spec, s.now().UTC())
	}
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	if record.Spec.OwnerID != owner || record.VersionID != versionID || record.ManifestDigest != version.ManifestDigest || record.Spec.Preset != preset {
		return PhotoWriteRecord{}, ErrConflict
	}
	if _, err := record.Spec.key(); err != nil {
		return PhotoWriteRecord{}, err
	}
	if !record.RetirementStartedAt.IsZero() || !record.RetiredAt.IsZero() {
		return PhotoWriteRecord{}, ErrDerivedRetired
	}
	if record.ObjectVersion != "" {
		if _, err := record.Receipt(); err != nil {
			return PhotoWriteRecord{}, err
		}
		return record, nil
	}
	var receipt SealedObject
	if !record.Spec.Multipart {
		receipt, err = storage.ReconcilePhotoCandidate(ctx, record.Spec)
	} else {
		preparer, ok := repository.(interface {
			PreparePhotoMultipartCompletion(context.Context, PhotoWriteRecord, time.Time) (PhotoWriteRecord, error)
		})
		if !ok {
			return PhotoWriteRecord{}, ErrPhotoCandidate
		}
		beforeComplete := func(call context.Context) error {
			var err error
			record, err = preparer.PreparePhotoMultipartCompletion(call, record, s.now().UTC())
			return err
		}
		if record.MultipartID == "" {
			binder, ok := repository.(interface {
				BindPhotoMultipart(context.Context, PhotoWriteRecord, string, time.Time) (PhotoWriteRecord, error)
			})
			if !ok {
				return PhotoWriteRecord{}, ErrPhotoCandidate
			}
			var id string
			if fresh {
				id, err = storage.StartPhotoMultipart(ctx, record.Spec)
			} else {
				id, err = storage.FindPhotoMultipart(ctx, record.Spec)
			}
			if err != nil {
				return PhotoWriteRecord{}, err
			}
			observation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			record, err = binder.BindPhotoMultipart(observation, record, id, s.now().UTC())
			cancel()
			if err != nil {
				return PhotoWriteRecord{}, err
			}
			if !record.RetirementStartedAt.IsZero() || !record.RetiredAt.IsZero() {
				return PhotoWriteRecord{}, ErrDerivedRetired
			}
			if record.ObjectVersion != "" {
				if _, err := record.Receipt(); err != nil {
					return PhotoWriteRecord{}, err
				}
				return record, nil
			}
		}
		if fresh {
			receipt, err = storage.StorePhotoMultipartOnce(ctx, record.Spec, record.MultipartID, candidate, beforeComplete)
		} else {
			receipt, err = storage.ReconcilePhotoMultipart(ctx, record.Spec, record.MultipartID, beforeComplete)
			if errors.Is(err, errPhotoPartsPending) {
				candidate, err = s.PhotoCandidate(ctx, owner, versionID, preset)
				if err == nil {
					receipt, err = storage.StorePhotoMultipartOnce(ctx, record.Spec, record.MultipartID, candidate, beforeComplete)
				}
			}
		}
	}
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	return repository.CompletePhotoWrite(ctx, record, receipt, s.now().UTC())
}
