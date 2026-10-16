package albums

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type DerivedCleanupRepository interface {
	PendingDerivedCleanups(context.Context, time.Time, int) ([]DerivedCleanup, error)
	ClaimDerivedCleanup(context.Context, string, string, string, time.Time) (DerivedCleanup, error)
	DerivedCleanup(context.Context, string, string) (DerivedCleanup, error)
	FinishDerivedCleanupStep(context.Context, DerivedCleanup, bool, time.Time) error
	StoreDerivedCleanupPlan(context.Context, DerivedCleanup, DerivedCleanupPlan, time.Time) (DerivedCleanup, error)
	FinishDerivedRetirement(context.Context, DerivedCleanup, time.Time) error
	AssetVersion(context.Context, string, string) (AssetVersion, error)
	PhotoWriteByAttempt(context.Context, string, string) (PhotoWriteRecord, error)
	CompletePhotoWrite(context.Context, PhotoWriteRecord, SealedObject, time.Time) (PhotoWriteRecord, error)
	VideoJob(context.Context, string, string, VideoPreset) (VideoJob, error)
	BindCleanupVideoSubmission(context.Context, DerivedCleanup, string, time.Time) (VideoJob, error)
	RecoverCleanupVideo(context.Context, DerivedCleanup, VideoResult, time.Time) (VideoJob, error)
}
type DerivedCleanupProvider interface {
	DerivedCleanupStorage
	VerifyVideoResult(context.Context, VideoJob) (VideoResult, error)
}
type DerivedCleanupBatch struct{ Recovered, Retired, Pruned, Waiting, Failed, Contended int }
type DerivedCleanupWorker struct {
	repository DerivedCleanupRepository
	storage    DerivedCleanupProvider
	batchSize  int
	now        func() time.Time
}

func NewDerivedCleanupWorker(repository DerivedCleanupRepository, storage DerivedCleanupProvider, batchSize int, now func() time.Time) (*DerivedCleanupWorker, error) {
	if repository == nil || storage == nil || batchSize < 1 || batchSize > 100 || now == nil {
		return nil, ErrInvalidManifest
	}
	return &DerivedCleanupWorker{repository, storage, batchSize, now}, nil
}

func (w *DerivedCleanupWorker) currentLease(ctx context.Context, expected DerivedCleanup) error {
	c, err := w.repository.DerivedCleanup(ctx, expected.OwnerID, expected.ID)
	if err != nil {
		return err
	}
	if !cleanupLease(c, expected, w.now().UTC()) {
		return ErrLease
	}
	return nil
}

// No PUT, transcode POST, cancellation or original-key DELETE occurs here.
// Ambiguous provider states retain bounded escrow; only terminal observations
// may be persisted as retirement plans before any version-specific delete.
func (w *DerivedCleanupWorker) advance(ctx context.Context, c DerivedCleanup) (string, error) {
	v, err := w.repository.AssetVersion(ctx, c.OwnerID, c.VersionID)
	if err != nil {
		return "", err
	}
	if v.VersionID != c.VersionID || v.ManifestDigest != c.ManifestDigest || validateVersionOwner(v, c.OwnerID) != nil {
		return "", ErrIncompleteBackup
	}
	var target DerivedCleanupTarget
	switch c.Kind {
	case "photo":
		p, err := w.repository.PhotoWriteByAttempt(ctx, c.OwnerID, c.ReferenceID)
		if err != nil {
			return "", err
		}
		if !cleanupPhotoMatches(c, p) || !p.RetiredAt.IsZero() || (v.Manifest.Kind != Photo && v.Manifest.Kind != LivePhoto) {
			return "", ErrConflict
		}
		if c.Plan != nil && c.KeepVersion == "" && p.RetirementStartedAt.IsZero() {
			return "", ErrConflict
		}
		target = DerivedCleanupTarget{"photo", c.OwnerID, c.ReferenceID, string(p.Spec.Preset)}
		if c.Plan == nil && c.KeepVersion == "" {
			versions, err := w.storage.ListDerivedVersions(ctx, target)
			if err != nil {
				return "", err
			}
			receipt, invalidCommitted, err := w.storage.AssessPhotoCleanup(ctx, p, versions)
			if err != nil {
				return "", err
			}
			if receipt != nil {
				if err := w.currentLease(ctx, c); err != nil {
					return "", err
				}
				if _, err := w.repository.CompletePhotoWrite(ctx, p, *receipt, w.now().UTC()); err != nil {
					return "", err
				}
				return "recovered", nil
			}
			if !invalidCommitted {
				return "", ErrDerivedPending
			}
			c, err = w.repository.StoreDerivedCleanupPlan(ctx, c, DerivedCleanupPlan{Terminal: true, Versions: versions}, w.now().UTC())
			if err != nil {
				return "", err
			}
		}
	case "video":
		j, err := w.repository.VideoJob(ctx, c.OwnerID, c.VersionID, VideoDaily1080)
		if err != nil {
			return "", err
		}
		source, err := videoSource(v)
		if err != nil || !cleanupVideoMatches(c, j) || !j.RetiredAt.IsZero() || source != j.Spec.Source {
			return "", ErrConflict
		}
		if c.Plan != nil && c.KeepVersion == "" && j.RetirementStartedAt.IsZero() {
			return "", ErrConflict
		}
		target = DerivedCleanupTarget{"video", c.OwnerID, c.ReferenceID, string(j.Preset)}
		if c.Plan == nil {
			observation, err := w.storage.InspectVideoCleanup(ctx, j)
			if err != nil {
				return "", err
			}
			if !observation.Terminal {
				return "", ErrDerivedPending
			}
			if j.ProviderJobID == "" {
				j, err = w.repository.BindCleanupVideoSubmission(ctx, c, observation.ProviderJobID, w.now().UTC())
				if err != nil {
					return "", err
				}
			} else if j.ProviderJobID != observation.ProviderJobID {
				return "", ErrConflict
			}
			if observation.Succeeded && c.KeepVersion == "" {
				result, err := w.storage.VerifyVideoResult(ctx, j)
				// Verification failure does not prove an invalid committed output;
				// retain escrow rather than discarding a possibly usable preview.
				if err != nil {
					return "", err
				}
				if _, err := w.repository.RecoverCleanupVideo(ctx, c, result, w.now().UTC()); err != nil {
					return "", err
				}
				return "recovered", nil
			}
			if c.KeepVersion != "" && !observation.Succeeded {
				return "", ErrConflict
			}
		}
	default:
		return "", ErrConflict
	}
	if _, err := target.key(); err != nil {
		return "", err
	}
	versions, err := w.storage.ListDerivedVersions(ctx, target)
	if err != nil {
		return "", err
	}
	if c.Plan == nil {
		c, err = w.repository.StoreDerivedCleanupPlan(ctx, c, DerivedCleanupPlan{Terminal: true, Versions: versions}, w.now().UTC())
		if err != nil {
			return "", err
		}
	}
	if err := cleanupSnapshotMatches(c, versions); err != nil {
		return "", err
	}
	for _, object := range versions {
		if object.VersionID == c.KeepVersion {
			continue
		}
		if err := w.currentLease(ctx, c); err != nil {
			return "", err
		}
		if err := w.storage.DeleteDerivedVersion(ctx, target, object); err != nil {
			return "", err
		}
	}
	remaining, err := w.storage.ListDerivedVersions(ctx, target)
	if err != nil {
		return "", err
	}
	if err := cleanupSnapshotMatches(c, remaining); err != nil {
		return "", err
	}
	for _, object := range remaining {
		if object.VersionID != c.KeepVersion {
			return "", ErrDerivedPending
		}
	}
	if err := w.repository.FinishDerivedRetirement(ctx, c, w.now().UTC()); err != nil {
		return "", err
	}
	if c.KeepVersion != "" {
		return "pruned", nil
	}
	return "retired", nil
}

func cleanupSnapshotMatches(c DerivedCleanup, observed []DerivedObjectVersion) error {
	if c.Plan == nil || validateCleanupPlan(*c.Plan) != nil || len(observed) > 1000 {
		return ErrConflict
	}
	planned := make(map[string]DerivedObjectVersion)
	for _, v := range c.Plan.Versions {
		planned[v.VersionID] = v
	}
	kept, seen := false, make(map[string]bool)
	for _, v := range observed {
		if seen[v.VersionID] || planned[v.VersionID] != v {
			return ErrConflict
		}
		seen[v.VersionID] = true
		if v.VersionID == c.KeepVersion {
			if v.DeleteMarker {
				return ErrConflict
			}
			kept = true
		}
	}
	if c.KeepVersion != "" && !kept {
		return ErrConflict
	}
	return nil
}

func (w *DerivedCleanupWorker) RunOnce(ctx context.Context) (DerivedCleanupBatch, error) {
	var b DerivedCleanupBatch
	entries, err := w.repository.PendingDerivedCleanups(ctx, w.now().UTC(), w.batchSize)
	if err != nil {
		return b, err
	}
	if len(entries) > w.batchSize {
		return b, ErrConflict
	}
	for _, pending := range entries {
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		c, err := w.repository.ClaimDerivedCleanup(ctx, pending.OwnerID, pending.ID, uuid.NewString(), w.now().UTC())
		if errors.Is(err, ErrLease) {
			b.Contended++
			continue
		}
		if err != nil {
			return b, err
		}
		operation, cancel := context.WithTimeout(ctx, 6*time.Minute)
		outcome, stepErr := w.advance(operation, c)
		cancel()
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		if stepErr == nil {
			switch outcome {
			case "recovered":
				b.Recovered++
			case "retired":
				b.Retired++
			case "pruned":
				b.Pruned++
			default:
				return b, ErrConflict
			}
			continue
		}
		if errors.Is(stepErr, ErrLease) {
			b.Contended++
			continue
		}
		terminal := errors.Is(stepErr, ErrConflict) || errors.Is(stepErr, ErrIncompleteBackup) || errors.Is(stepErr, ErrInvalidManifest) || errors.Is(stepErr, ErrNotFound) || errors.Is(stepErr, ErrDerivedRetired)
		if err := w.repository.FinishDerivedCleanupStep(ctx, c, terminal, w.now().UTC()); errors.Is(err, ErrLease) {
			b.Contended++
			continue
		} else if err != nil {
			return b, err
		}
		if errors.Is(stepErr, ErrDerivedPending) {
			b.Waiting++
		} else {
			b.Failed++
		}
	}
	return b, nil
}

func (w *DerivedCleanupWorker) Run(ctx context.Context, interval time.Duration, observe func(DerivedCleanupBatch, bool)) error {
	if interval < time.Second || interval > time.Minute {
		return ErrInvalidManifest
	}
	for {
		b, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if observe != nil {
			observe(b, err == nil)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var _ DerivedCleanupRepository = (*MySQLUploads)(nil)
var _ DerivedCleanupProvider = (*COSObjects)(nil)
