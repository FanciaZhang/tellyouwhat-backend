package albums

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

func (s *COSObjects) CheckOriginalAvailability(ctx context.Context, object SealedObject) error {
	if !albumObjectKey(object.Key, "originals") || !strings.HasSuffix(object.Key, "/"+object.ResourceID) ||
		!concreteCOSVersion(object.VersionID) || object.SizeBytes <= 0 {
		return ErrIncompleteBackup
	}
	response, err := s.client.Object.Head(ctx, object.Key, nil, url.QueryEscape(object.VersionID))
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		return ErrCOSOperation
	}
	if response.Header.Get("x-cos-version-id") != object.VersionID || response.ContentLength != object.SizeBytes ||
		response.Header.Get("x-cos-delete-marker") != "" && response.Header.Get("x-cos-delete-marker") != "false" {
		return ErrIncompleteBackup
	}
	// HEAD also succeeds for offline archive objects. Only the online classes
	// used by Albums qualify; a temporary restore is not durable online storage.
	switch response.Header.Get("x-cos-storage-class") {
	case "", "STANDARD", "STANDARD_IA", "MAZ_STANDARD", "MAZ_STANDARD_IA":
		return nil
	default:
		return ErrReadUnavailable
	}
}
