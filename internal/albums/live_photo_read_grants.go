package albums

import (
	"context"
	"errors"
	"time"
)

var ErrLivePhotoNotReady = errors.New("album live photo components are not ready")

// The JPEG and MP4 are verified immutable components of one source version.
// They still need native pairing and system-library readback. No deletion or
// finished cloud-pair certificate can be inferred from this read grant.
type LivePhotoPreviewReadGrant struct {
	AssetID             string                `json:"assetID"`
	VersionID           string                `json:"versionID"`
	ManifestDigest      string                `json:"manifestDigest"`
	Timing              LivePhotoTiming       `json:"timing"`
	Photo               PhotoPreviewReadGrant `json:"photo"`
	Video               VideoPreviewReadGrant `json:"video"`
	ReplacementEligible bool                  `json:"replacementEligible"`
	ExpiresAt           time.Time             `json:"expiresAt"`
}

func (s *UploadService) LivePhotoPreviewReadGrant(ctx context.Context, owner, version string) (LivePhotoPreviewReadGrant, error) {
	if !validOwner(owner) || !validOwner(version) {
		return LivePhotoPreviewReadGrant{}, ErrInvalidManifest
	}
	v, err := s.verifiedAssetVersion(ctx, owner, version)
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	if !livePhotoPreviewEligible(v.Manifest) {
		return LivePhotoPreviewReadGrant{}, ErrNotFound
	}
	pr, hasPhoto := s.repository.(PhotoWriteRepository)
	vr, hasVideo := s.repository.(VideoJobRepository)
	if !hasPhoto || !hasVideo {
		return LivePhotoPreviewReadGrant{}, ErrReadUnavailable
	}
	// Check both receipts before minting either URL. A still-only result cannot
	// accidentally become a playable or replaceable Live Photo in the client.
	p, err := pr.PhotoWrite(ctx, owner, version, PhotoDailyJPEG)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrPhotoWriteUncertain) {
		return LivePhotoPreviewReadGrant{}, ErrLivePhotoNotReady
	}
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	if p.Spec.OwnerID != owner || p.VersionID != version || p.ManifestDigest != v.ManifestDigest || p.Spec.Preset != PhotoDailyJPEG {
		return LivePhotoPreviewReadGrant{}, ErrConflict
	}
	if _, err := p.Receipt(); err != nil {
		return LivePhotoPreviewReadGrant{}, ErrLivePhotoNotReady
	}
	j, err := vr.VideoJob(ctx, owner, version, VideoDaily1080)
	if errors.Is(err, ErrNotFound) {
		return LivePhotoPreviewReadGrant{}, ErrLivePhotoNotReady
	}
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	source, err := videoSource(v)
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	if j.OwnerID != owner || j.VersionID != version || j.ManifestDigest != v.ManifestDigest || j.Preset != VideoDaily1080 || (j.Spec != nil && j.Spec.Source != source) {
		return LivePhotoPreviewReadGrant{}, ErrConflict
	}
	if j.State != "completed" || j.Result == nil {
		return LivePhotoPreviewReadGrant{}, ErrLivePhotoNotReady
	}
	if j.validateResult(*j.Result) != nil || !sameLiveTiming(j.Spec.LivePhotoTiming, v.Manifest.LivePhotoTiming) || !livePhotoProfileMatches(v.Manifest, j.Result.Profile) || !ciJobPattern.MatchString(j.ProviderJobID) || j.SubmittedAt.IsZero() {
		return LivePhotoPreviewReadGrant{}, ErrConflict
	}
	photo, err := s.PhotoPreviewReadGrant(ctx, owner, version, PhotoDailyJPEG)
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	video, err := s.VideoPreviewReadGrant(ctx, owner, version, VideoDaily1080)
	if err != nil {
		return LivePhotoPreviewReadGrant{}, err
	}
	if photo.ManifestDigest != v.ManifestDigest || video.ManifestDigest != v.ManifestDigest {
		return LivePhotoPreviewReadGrant{}, ErrConflict
	}
	expiry := photo.ExpiresAt
	if video.ExpiresAt.Before(expiry) {
		expiry = video.ExpiresAt
	}
	return LivePhotoPreviewReadGrant{AssetID: v.AssetID, VersionID: version, ManifestDigest: v.ManifestDigest,
		Timing: *v.Manifest.LivePhotoTiming, Photo: photo, Video: video, ExpiresAt: expiry}, nil
}
