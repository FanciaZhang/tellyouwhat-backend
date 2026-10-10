package albums

import "context"

type PhotoCandidateRenderer interface {
	RenderPhotoCandidate(context.Context, SealedObject, PhotoPreset) (PhotoCandidate, error)
}

// PhotoCandidate renders the displayed still of a verified version. It is an
// internal worker entry point: no HTTP route, free-form rule, input URL or
// user-selected object key is exposed. A Live Photo still is only a component;
// its successful render does not establish a playable daily Live Photo pair.
func (s *UploadService) PhotoCandidate(ctx context.Context, owner, versionID string, preset PhotoPreset) (PhotoCandidate, error) {
	if rule, _, _, _ := photoPresetRule(preset); rule == "" {
		return PhotoCandidate{}, ErrInvalidManifest
	}
	renderer, ok := s.objects.(PhotoCandidateRenderer)
	if !ok {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	version, err := s.verifiedAssetVersion(ctx, owner, versionID)
	if err != nil {
		return PhotoCandidate{}, err
	}
	if version.Manifest.Kind != Photo && version.Manifest.Kind != LivePhoto {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	role := OriginalPhoto
	if version.Manifest.Edited {
		role = CurrentPhoto
	}
	var resourceID string
	for _, resource := range version.Manifest.Resources {
		if resource.Role == role {
			resourceID = resource.ID
		}
	}
	for _, object := range version.Objects {
		if object.ResourceID == resourceID {
			return renderer.RenderPhotoCandidate(ctx, object, preset)
		}
	}
	return PhotoCandidate{}, ErrIncompleteBackup
}
