package albums

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrReadUnavailable = errors.New("album original read authorization unavailable")

// OriginalReadGrant authorizes one immutable object. It is not a public share
// link and must never be logged, persisted in the catalogue, or used as backup proof.
type OriginalReadGrant struct {
	AssetID         string            `json:"assetID"`
	VersionID       string            `json:"versionID"`
	ManifestDigest  string            `json:"manifestDigest"`
	ResourceID      string            `json:"resourceID"`
	ObjectVersionID string            `json:"objectVersionID"`
	SizeBytes       int64             `json:"sizeBytes"`
	SHA256          string            `json:"sha256"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	ExpiresAt       time.Time         `json:"expiresAt"`
}

type OriginalReadAuthorizer interface {
	AuthorizeOriginalRead(context.Context, SealedObject, time.Time) (string, error)
}

func (s *UploadService) OriginalReadGrant(ctx context.Context, owner, versionID, resourceID string) (OriginalReadGrant, error) {
	if !validOwner(owner) || !validOwner(versionID) || !resourceIDPattern.MatchString(resourceID) {
		return OriginalReadGrant{}, ErrInvalidManifest
	}
	signer, supported := s.objects.(OriginalReadAuthorizer)
	if !supported {
		return OriginalReadGrant{}, ErrReadUnavailable
	}
	version, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return OriginalReadGrant{}, err
	}
	var selected *SealedObject
	for _, object := range version.Objects {
		if object.ResourceID == resourceID {
			copy := object
			selected = &copy
		}
	}
	if selected == nil {
		return OriginalReadGrant{}, ErrNotFound
	}
	var signed string
	expiresAt, err := s.authorizeLibraryRead(ctx, owner, version, func(expiry time.Time) error {
		var err error
		signed, err = signer.AuthorizeOriginalRead(ctx, *selected, expiry)
		return err
	})
	if err != nil {
		return OriginalReadGrant{}, err
	}
	return OriginalReadGrant{AssetID: version.AssetID, VersionID: versionID, ManifestDigest: version.ManifestDigest,
		ResourceID: resourceID, ObjectVersionID: selected.VersionID, SizeBytes: selected.SizeBytes,
		SHA256: strings.ToLower(selected.SHA256), URL: signed, Headers: map[string]string{}, ExpiresAt: expiresAt}, nil
}

// verifiedAssetVersion is shared by reads and processing. A catalogue record
// alone cannot authorize consumption of a partial or cross-account resource set.
func (s *UploadService) verifiedAssetVersion(ctx context.Context, owner, versionID string) (AssetVersion, error) {
	if !validOwner(owner) || !validOwner(versionID) {
		return AssetVersion{}, ErrInvalidManifest
	}
	repository, ok := s.repository.(LibraryRepository)
	if !ok {
		return AssetVersion{}, ErrReadUnavailable
	}
	var version AssetVersion
	var err error
	if visible, ok := repository.(interface {
		VisibleAssetVersion(context.Context, string, string) (AssetVersion, error)
	}); ok {
		version, err = visible.VisibleAssetVersion(ctx, owner, versionID)
	} else {
		version, err = repository.AssetVersion(ctx, owner, versionID)
	}
	if err != nil {
		return AssetVersion{}, err
	}
	if err := validateReadableVersion(version, owner, versionID); err != nil {
		return AssetVersion{}, err
	}
	return version, nil
}

func validateReadableVersion(version AssetVersion, owner, versionID string) error {
	if version.VersionID != versionID || version.Sequence <= 0 || version.AssetID != version.Manifest.AssetID {
		return ErrIncompleteBackup
	}
	if err := validateVersionOwner(version, owner); err != nil {
		return err
	}
	observations := make([]Observation, 0, len(version.Objects))
	for _, object := range version.Objects {
		if !concreteCOSVersion(object.VersionID) {
			return ErrIncompleteBackup
		}
		observations = append(observations, Observation{object.ResourceID, object.SizeBytes, object.SHA256})
	}
	digest, err := VerifyBackup(version.Manifest, observations)
	if err != nil || digest != version.ManifestDigest {
		return ErrIncompleteBackup
	}
	var total int64
	for _, resource := range version.Manifest.Resources {
		total += resource.SizeBytes
	}
	if total != version.OriginalBytes {
		return ErrIncompleteBackup
	}
	return nil
}
