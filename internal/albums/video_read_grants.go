package albums

import (
	"context"
	"errors"
	"time"
)

var ErrVideoNotReady = errors.New("album video preview is not ready")

type VideoPreviewReadGrant struct {
	AssetID             string            `json:"assetID"`
	VersionID           string            `json:"versionID"`
	ManifestDigest      string            `json:"manifestDigest"`
	Preset              VideoPreset       `json:"preset"`
	ObjectVersionID     string            `json:"objectVersionID"`
	SizeBytes           int64             `json:"sizeBytes"`
	SHA256              string            `json:"sha256"`
	Profile             VideoProfile      `json:"profile"`
	ContentType         string            `json:"contentType"`
	ReplacementEligible bool              `json:"replacementEligible"`
	URL                 string            `json:"url"`
	Headers             map[string]string `json:"headers"`
	ExpiresAt           time.Time         `json:"expiresAt"`
}

type VideoPreviewReadAuthorizer interface {
	AuthorizeVideoPreviewRead(context.Context, VideoJob, time.Time) (string, error)
}

func (s *UploadService) VideoPreviewReadGrant(ctx context.Context, owner, versionID string, preset VideoPreset) (VideoPreviewReadGrant, error) {
	if !validOwner(owner) || !validOwner(versionID) || preset != VideoDaily1080 {
		return VideoPreviewReadGrant{}, ErrInvalidManifest
	}
	repository, ok := s.repository.(VideoJobRepository)
	signer, supported := s.objects.(VideoPreviewReadAuthorizer)
	if !ok || !supported {
		return VideoPreviewReadGrant{}, ErrReadUnavailable
	}
	v, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return VideoPreviewReadGrant{}, err
	}
	source, err := videoSource(v)
	if err != nil {
		return VideoPreviewReadGrant{}, ErrNotFound
	}
	j, err := repository.VideoJob(ctx, owner, versionID, preset)
	if err != nil {
		return VideoPreviewReadGrant{}, err
	}
	if j.OwnerID != owner || j.VersionID != versionID || j.Preset != preset || j.ManifestDigest != v.ManifestDigest || (j.Spec != nil && j.Spec.Source != source) {
		return VideoPreviewReadGrant{}, ErrConflict
	}
	if j.State != "completed" || j.Result == nil {
		return VideoPreviewReadGrant{}, ErrVideoNotReady
	}
	if j.validateResult(*j.Result) != nil || !ciJobPattern.MatchString(j.ProviderJobID) || j.SubmittedAt.IsZero() {
		return VideoPreviewReadGrant{}, ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return VideoPreviewReadGrant{}, err
	}
	expiry := s.now().UTC().Add(5 * time.Minute)
	signed, err := signer.AuthorizeVideoPreviewRead(ctx, j, expiry)
	if err != nil {
		return VideoPreviewReadGrant{}, err
	}
	object := j.Result.Object
	return VideoPreviewReadGrant{AssetID: v.AssetID, VersionID: versionID, ManifestDigest: v.ManifestDigest, Preset: preset,
		ObjectVersionID: object.VersionID, SizeBytes: object.SizeBytes, SHA256: object.SHA256, Profile: j.Result.Profile, ContentType: "video/mp4",
		ReplacementEligible: false, URL: signed, Headers: map[string]string{}, ExpiresAt: expiry}, nil
}

func (s *COSObjects) AuthorizeVideoPreviewRead(ctx context.Context, j VideoJob, expiry time.Time) (string, error) {
	if j.State != "completed" || j.Result == nil || j.validateResult(*j.Result) != nil {
		return "", ErrVideoNotReady
	}
	return s.signImmutableRead(ctx, j.Result.Object.Key, j.Result.Object.VersionID, expiry)
}
