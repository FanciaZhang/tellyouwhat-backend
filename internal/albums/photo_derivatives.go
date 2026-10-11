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
	StorePhotoCandidateOnce(context.Context, PhotoWriteSpec, PhotoCandidate) (SealedObject, error)
	ReconcilePhotoCandidate(context.Context, PhotoWriteSpec) (SealedObject, error)
}

// GeneratePhotoDerivative is an internal durable operation. It resumes an
// existing attempt before rendering, so restart recovery neither re-encodes nor
// blindly writes a new version. Returned storage evidence is not authorization
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
	if version.Manifest.Kind != Photo && version.Manifest.Kind != LivePhoto {
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
		spec := PhotoWriteSpec{OwnerID: owner, AttemptID: uuid.NewString(), Preset: preset, SizeBytes: int64(len(candidate.Bytes)), SHA256: candidate.SHA256, Width: candidate.Width, Height: candidate.Height}
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
	if fresh {
		receipt, err = storage.StorePhotoCandidateOnce(ctx, record.Spec, candidate)
	} else {
		receipt, err = storage.ReconcilePhotoCandidate(ctx, record.Spec)
	}
	if err != nil {
		return PhotoWriteRecord{}, err
	}
	return repository.CompletePhotoWrite(ctx, record, receipt, s.now().UTC())
}
