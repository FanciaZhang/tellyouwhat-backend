package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type DerivedCleanupTarget struct{ Kind, OwnerID, ReferenceID, Preset string }

func (t DerivedCleanupTarget) key() (string, error) {
	if !validOwner(t.OwnerID) || !validOwner(t.ReferenceID) {
		return "", ErrInvalidManifest
	}
	switch t.Kind {
	case "photo":
		if rule, _, _, _ := photoPresetRule(PhotoPreset(t.Preset)); rule != "" {
			return "albums/derivatives/" + t.OwnerID + "/" + t.ReferenceID + "/" + t.Preset, nil
		}
	case "video":
		if VideoPreset(t.Preset) == VideoDaily1080 {
			return "albums/video-previews/" + t.OwnerID + "/" + t.ReferenceID + "/" + t.Preset, nil
		}
	}
	return "", ErrInvalidManifest
}

type VideoCleanupObservation struct {
	ProviderJobID       string
	Terminal, Succeeded bool
}
type DerivedCleanupStorage interface {
	ListDerivedVersions(context.Context, DerivedCleanupTarget) ([]DerivedObjectVersion, error)
	DeleteDerivedVersion(context.Context, DerivedCleanupTarget, DerivedObjectVersion) error
	AssessPhotoCleanup(context.Context, PhotoWriteRecord, []DerivedObjectVersion) (*SealedObject, bool, error)
	InspectVideoCleanup(context.Context, VideoJob) (VideoCleanupObservation, error)
}

// A complete bounded list is required before any delete. Prefix neighbours are
// never targets; both current/historical versions and delete markers are listed.
func (s *COSObjects) ListDerivedVersions(ctx context.Context, t DerivedCleanupTarget) ([]DerivedObjectVersion, error) {
	key, err := t.key()
	if err != nil {
		return nil, err
	}
	bucket := strings.Split(s.host, ".cos.")[0]
	marker, versionMarker := "", ""
	seenPages := map[string]bool{}
	seenVersions := map[string]bool{}
	var versions []DerivedObjectVersion
	for range 5 {
		result, response, err := s.client.Bucket.GetObjectVersions(ctx, &cos.BucketGetObjectVersionsOptions{Prefix: key, MaxKeys: 1000, KeyMarker: marker, VersionIdMarker: versionMarker})
		if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil {
			return nil, ErrCOSOperation
		}
		if result.Name != bucket || result.Prefix != key || result.EncodingType != "" || result.Delimiter != "" || len(result.CommonPrefixes) != 0 ||
			result.KeyMarker != marker || result.VersionIdMarker != versionMarker || len(result.Version)+len(result.DeleteMarker) > 1000 {
			return nil, ErrConflict
		}
		add := func(objectKey, id string, size int64, deleted bool) error {
			if !strings.HasPrefix(objectKey, key) || !concreteCOSVersion(id) || size < 0 {
				return ErrConflict
			}
			if objectKey != key {
				return nil
			}
			if seenVersions[id] || len(versions) >= 1000 {
				return ErrConflict
			}
			seenVersions[id] = true
			versions = append(versions, DerivedObjectVersion{id, deleted, size})
			return nil
		}
		for _, v := range result.Version {
			if err := add(v.Key, v.VersionId, v.Size, false); err != nil {
				return nil, err
			}
		}
		for _, v := range result.DeleteMarker {
			if err := add(v.Key, v.VersionId, 0, true); err != nil {
				return nil, err
			}
		}
		if !result.IsTruncated {
			return versions, nil
		}
		if !strings.HasPrefix(result.NextKeyMarker, key) || !concreteCOSVersion(result.NextVersionIdMarker) {
			return nil, ErrConflict
		}
		next := result.NextKeyMarker + "\x00" + result.NextVersionIdMarker
		if seenPages[next] || (result.NextKeyMarker == marker && result.NextVersionIdMarker == versionMarker) {
			return nil, ErrConflict
		}
		seenPages[next] = true
		marker, versionMarker = result.NextKeyMarker, result.NextVersionIdMarker
	}
	return nil, ErrConflict
}

// A versionId query is mandatory. A versionless DELETE would create a marker
// and keep historical bytes billed; it is never used by this cleanup path.
func (s *COSObjects) DeleteDerivedVersion(ctx context.Context, t DerivedCleanupTarget, v DerivedObjectVersion) error {
	key, err := t.key()
	if err != nil || !concreteCOSVersion(v.VersionID) {
		return ErrInvalidManifest
	}
	query := url.Values{"versionId": []string{v.VersionID}}
	response, err := s.client.Object.Delete(ctx, key, &cos.ObjectDeleteOptions{XOptionQuery: &query})
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		var provider *cos.ErrorResponse
		if response != nil && response.StatusCode == http.StatusNotFound && errors.As(err, &provider) &&
			(provider.Code == "NoSuchKey" || provider.Code == "NoSuchVersion") {
			return nil
		}
		return ErrCOSOperation
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return ErrCOSOperation
	}
	if id := response.Header.Get("x-cos-version-id"); id != "" && id != v.VersionID {
		return ErrCOSOperation
	}
	return nil
}

// Positive, fixed-version observations distinguish an invalid committed image
// from network uncertainty. Missing/multiple versions never prove a lost PUT
// did not happen; do not release its escrow or discard a potentially valid copy.
func (s *COSObjects) AssessPhotoCleanup(ctx context.Context, p PhotoWriteRecord, versions []DerivedObjectVersion) (*SealedObject, bool, error) {
	key, err := p.Spec.key()
	if err != nil || p.ObjectVersion != "" || !p.RetirementStartedAt.IsZero() {
		return nil, false, ErrConflict
	}
	var objects []DerivedObjectVersion
	for _, v := range versions {
		if !v.DeleteMarker {
			objects = append(objects, v)
		}
	}
	if len(objects) == 0 {
		return nil, false, nil
	}
	if len(objects) != 1 {
		return nil, false, ErrConflict
	}
	v := objects[0]
	if !concreteCOSVersion(v.VersionID) || v.SizeBytes < 0 {
		return nil, false, ErrConflict
	}
	if v.SizeBytes != p.Spec.SizeBytes {
		return nil, true, nil
	}
	response, err := s.client.Object.Get(ctx, key, nil, url.QueryEscape(v.VersionID))
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != v.VersionID ||
		response.ContentLength != p.Spec.SizeBytes || response.Header.Get("Content-Range") != "" || response.Uncompressed ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return nil, false, ErrCOSOperation
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, p.Spec.SizeBytes+1))
	if err != nil || int64(len(data)) != p.Spec.SizeBytes || ctx.Err() != nil {
		return nil, false, ErrCOSOperation
	}
	candidate, err := inspectPhotoCandidate(data, p.Spec.Preset)
	if err != nil || candidate.SHA256 != p.Spec.SHA256 || candidate.Width != p.Spec.Width || candidate.Height != p.Spec.Height {
		return nil, true, nil
	}
	return &SealedObject{ResourceID: string(p.Spec.Preset), Key: key, VersionID: v.VersionID, SizeBytes: p.Spec.SizeBytes, SHA256: p.Spec.SHA256}, false, nil
}
func (s *COSObjects) InspectVideoCleanup(ctx context.Context, j VideoJob) (VideoCleanupObservation, error) {
	if j.Spec == nil || j.validateSpec(*j.Spec) != nil || j.SubmittedAt.IsZero() {
		return VideoCleanupObservation{}, ErrInvalidManifest
	}
	id := j.ProviderJobID
	if id == "" {
		var err error
		id, err = s.FindVideoSubmission(ctx, j)
		if err != nil {
			return VideoCleanupObservation{}, err
		}
		if id == "" {
			return VideoCleanupObservation{}, nil
		}
	}
	if !ciJobPattern.MatchString(id) {
		return VideoCleanupObservation{}, ErrConflict
	}
	result, response, err := s.client.CI.DescribeMediaJob(ctx, id)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || result == nil {
		return VideoCleanupObservation{}, ErrVideoProcessing
	}
	if !s.matchesVideoJob(j, result.JobsDetail) || result.JobsDetail.JobId != id {
		return VideoCleanupObservation{}, ErrConflict
	}
	observation := VideoCleanupObservation{ProviderJobID: id}
	switch result.JobsDetail.State {
	case "Submitted", "Running", "Pause":
		return observation, nil
	case "Success":
		observation.Terminal, observation.Succeeded = true, true
	case "Failed", "Cancel":
		observation.Terminal = true
	default:
		return VideoCleanupObservation{}, ErrVideoUncertain
	}
	return observation, nil
}
