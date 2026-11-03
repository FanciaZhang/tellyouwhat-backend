package albums

import (
	"context"
	"time"
)

// A point-in-time check, not a deletion permit or a reservation against purge.
// Full-byte verification belongs to publication; this checks those exact sealed
// versions still exist in online storage without downloading originals again.
type OriginalAvailability struct {
	AssetID        string    `json:"assetID"`
	VersionID      string    `json:"versionID"`
	ManifestDigest string    `json:"manifestDigest"`
	SourceRevision string    `json:"sourceRevision"`
	OriginalBytes  int64     `json:"originalBytes"`
	ResourceCount  int       `json:"resourceCount"`
	StartedAt      time.Time `json:"startedAt"`
	CheckedAt      time.Time `json:"checkedAt"`
}

type originalAvailabilityChecker interface {
	CheckOriginalAvailability(context.Context, SealedObject) error
}

func (s *UploadService) CheckOriginalAvailability(ctx context.Context, owner, versionID string) (OriginalAvailability, error) {
	if !validOwner(owner) || !validOwner(versionID) {
		return OriginalAvailability{}, ErrInvalidManifest
	}
	checker, ok := s.objects.(originalAvailabilityChecker)
	if !ok {
		return OriginalAvailability{}, ErrReadUnavailable
	}
	// Unlike a read URL, this check requires the repository's locked lifecycle
	// and retention checks. There is no permissive legacy repository fallback.
	if _, ok := s.repository.(cloudReadGrantAuthorizer); !ok {
		return OriginalAvailability{}, ErrReadUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	started := s.now().UTC()
	v, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return OriginalAvailability{}, err
	}
	checkAccess := func() error {
		_, err := s.authorizeLibraryRead(ctx, owner, v, func(time.Time) error { return nil })
		return err
	}
	if err := checkAccess(); err != nil {
		return OriginalAvailability{}, err
	}
	for _, object := range v.Objects {
		if err := ctx.Err(); err != nil {
			return OriginalAvailability{}, err
		}
		if err := checker.CheckOriginalAvailability(ctx, object); err != nil {
			return OriginalAvailability{}, err
		}
	}
	// Storage I/O is outside the account lock. Recheck after all resources so
	// trash, account deletion or retention expiry during I/O cannot report success.
	if err := checkAccess(); err != nil {
		return OriginalAvailability{}, err
	}
	checked := s.now().UTC()
	if err := ctx.Err(); err != nil {
		return OriginalAvailability{}, err
	}
	if checked.Before(started) || checked.Sub(started) >= 30*time.Second {
		return OriginalAvailability{}, ErrReadUnavailable
	}
	return OriginalAvailability{v.AssetID, v.VersionID, v.ManifestDigest, v.Manifest.SourceRevision,
		v.OriginalBytes, len(v.Objects), started, checked}, nil
}
