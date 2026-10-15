package albums

import (
	"context"
	"errors"
	"time"
)

// PhotoPreviewReadGrant describes a verified stored still, never a replacement
// certificate. A Live Photo's still preview does not replace its paired video.
// Clients must verify downloaded bytes against this descriptor before display.
type PhotoPreviewReadGrant struct {
	AssetID             string            `json:"assetID"`
	VersionID           string            `json:"versionID"`
	ManifestDigest      string            `json:"manifestDigest"`
	Preset              PhotoPreset       `json:"preset"`
	ObjectVersionID     string            `json:"objectVersionID"`
	SizeBytes           int64             `json:"sizeBytes"`
	SHA256              string            `json:"sha256"`
	Width               int               `json:"width"`
	Height              int               `json:"height"`
	ContentType         string            `json:"contentType"`
	ReplacementEligible bool              `json:"replacementEligible"`
	URL                 string            `json:"url"`
	Headers             map[string]string `json:"headers"`
	ExpiresAt           time.Time         `json:"expiresAt"`
}

type PhotoPreviewReadAuthorizer interface {
	AuthorizePhotoPreviewRead(context.Context, PhotoWriteRecord, time.Time) (string, error)
}

func (s *UploadService) PhotoPreviewReadGrant(ctx context.Context, owner, versionID string, preset PhotoPreset) (PhotoPreviewReadGrant, error) {
	_, format, _, _ := photoPresetRule(preset)
	if !validOwner(owner) || !validOwner(versionID) || format == "" {
		return PhotoPreviewReadGrant{}, ErrInvalidManifest
	}
	repository, ok := s.repository.(PhotoWriteRepository)
	signer, supported := s.objects.(PhotoPreviewReadAuthorizer)
	if !ok || !supported {
		return PhotoPreviewReadGrant{}, ErrReadUnavailable
	}
	version, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return PhotoPreviewReadGrant{}, err
	}
	if version.Manifest.Kind != Photo && version.Manifest.Kind != LivePhoto {
		return PhotoPreviewReadGrant{}, ErrNotFound
	}
	record, err := repository.PhotoWrite(ctx, owner, versionID, preset)
	// Publication precedes preview rendering. The owned original exists even
	// when the worker has not created its derivative receipt yet.
	if errors.Is(err, ErrNotFound) {
		return PhotoPreviewReadGrant{}, ErrPhotoWriteUncertain
	}
	if err != nil {
		return PhotoPreviewReadGrant{}, err
	}
	if record.Spec.OwnerID != owner || record.VersionID != versionID || record.ManifestDigest != version.ManifestDigest || record.Spec.Preset != preset {
		return PhotoPreviewReadGrant{}, ErrConflict
	}
	receipt, err := record.Receipt()
	if err != nil {
		return PhotoPreviewReadGrant{}, err
	}
	if err := ctx.Err(); err != nil {
		return PhotoPreviewReadGrant{}, err
	}
	var signed string
	expiry, err := s.authorizeLibraryRead(ctx, owner, version, func(expiry time.Time) error {
		var err error
		signed, err = signer.AuthorizePhotoPreviewRead(ctx, record, expiry)
		return err
	})
	if err != nil {
		return PhotoPreviewReadGrant{}, err
	}
	return PhotoPreviewReadGrant{AssetID: version.AssetID, VersionID: versionID, ManifestDigest: version.ManifestDigest,
		Preset: preset, ObjectVersionID: receipt.VersionID, SizeBytes: receipt.SizeBytes, SHA256: receipt.SHA256,
		Width: record.Spec.Width, Height: record.Spec.Height, ContentType: "image/" + format,
		ReplacementEligible: false, URL: signed, Headers: map[string]string{}, ExpiresAt: expiry}, nil
}

// Signing uses only an already completed receipt. It never invokes CI, retries
// an upload or reads the latest object version while serving a browsing request.
func (s *COSObjects) AuthorizePhotoPreviewRead(ctx context.Context, record PhotoWriteRecord, expiry time.Time) (string, error) {
	receipt, err := record.Receipt()
	if err != nil {
		return "", err
	}
	return s.signImmutableRead(ctx, receipt.Key, receipt.VersionID, expiry)
}
