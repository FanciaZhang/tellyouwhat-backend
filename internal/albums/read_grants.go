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
	repository, ok := s.repository.(LibraryRepository)
	signer, supported := s.objects.(OriginalReadAuthorizer)
	if !ok || !supported {
		return OriginalReadGrant{}, ErrReadUnavailable
	}
	version, err := repository.AssetVersion(ctx, owner, versionID)
	if err != nil {
		return OriginalReadGrant{}, err
	}
	if version.VersionID != versionID || version.Sequence <= 0 || version.AssetID != version.Manifest.AssetID {
		return OriginalReadGrant{}, ErrIncompleteBackup
	}
	if err := validateVersionOwner(version, owner); err != nil {
		return OriginalReadGrant{}, err
	}
	observations := make([]Observation, 0, len(version.Objects))
	var selected *SealedObject
	for _, object := range version.Objects {
		if !concreteCOSVersion(object.VersionID) {
			return OriginalReadGrant{}, ErrIncompleteBackup
		}
		observations = append(observations, Observation{object.ResourceID, object.SizeBytes, object.SHA256})
		if object.ResourceID == resourceID {
			copy := object
			selected = &copy
		}
	}
	digest, err := VerifyBackup(version.Manifest, observations)
	if err != nil || digest != version.ManifestDigest {
		return OriginalReadGrant{}, ErrIncompleteBackup
	}
	var total int64
	for _, resource := range version.Manifest.Resources {
		total += resource.SizeBytes
	}
	if total != version.OriginalBytes {
		return OriginalReadGrant{}, ErrIncompleteBackup
	}
	if selected == nil {
		return OriginalReadGrant{}, ErrNotFound
	}
	expiresAt := s.now().UTC().Add(5 * time.Minute)
	signed, err := signer.AuthorizeOriginalRead(ctx, *selected, expiresAt)
	if err != nil {
		return OriginalReadGrant{}, err
	}
	return OriginalReadGrant{AssetID: version.AssetID, VersionID: versionID, ManifestDigest: digest,
		ResourceID: resourceID, ObjectVersionID: selected.VersionID, SizeBytes: selected.SizeBytes,
		SHA256: strings.ToLower(selected.SHA256), URL: signed, Headers: map[string]string{}, ExpiresAt: expiresAt}, nil
}
