package albums

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func (s *COSObjects) listPurgeVersions(ctx context.Context, owner string, t purgeTarget) ([]DerivedObjectVersion, error) {
	if !t.valid(owner) {
		return nil, ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A positive bounded batch is enough to make progress. An empty batch must
	// exhaust pagination; never infer absence from one page or a page limit.
	return s.listObjectVersions(ctx, t.Key, true)
}

func (s *COSObjects) deletePurgeVersion(ctx context.Context, owner string, t purgeTarget, v DerivedObjectVersion) error {
	if !t.valid(owner) || validateCleanupPlan(DerivedCleanupPlan{Terminal: true, Versions: []DerivedObjectVersion{v}}) != nil {
		return ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.deleteObjectVersion(ctx, t.Key, v.VersionID)
}

func (s *COSObjects) listPurgeUploads(ctx context.Context, owner string, t purgeTarget) ([]string, error) {
	if !t.valid(owner) {
		return nil, ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keyMarker, idMarker := "", ""
	seen := map[string]bool{}
	for range 5 {
		result, err := s.readMultipartInventory(ctx, t.Key, keyMarker, idMarker)
		if err != nil {
			return nil, ErrCOSOperation
		}
		if result.Bucket != strings.Split(s.host, ".cos.")[0] || result.Prefix != t.Key || result.EncodingType != "" || result.Delimiter != "" ||
			len(result.CommonPrefixes) != 0 || result.IsTruncated == nil || result.MaxUploads != 1000 || len(result.Uploads) > 1000 || result.KeyMarker != keyMarker || result.UploadIDMarker != idMarker {
			return nil, ErrConflict
		}
		var ids []string
		for _, u := range result.Uploads {
			identity := u.Key + "\x00" + u.UploadID
			if !strings.HasPrefix(u.Key, t.Key) || !validCOSMultipartID(u.UploadID) || seen[identity] {
				return nil, ErrConflict
			}
			seen[identity] = true
			if u.Key == t.Key {
				ids = append(ids, u.UploadID)
			}
		}
		if len(ids) > 0 || !*result.IsTruncated {
			return ids, nil
		}
		if len(result.Uploads) == 0 {
			return nil, ErrConflict
		}
		last := result.Uploads[len(result.Uploads)-1]
		if result.NextKeyMarker != last.Key || result.NextUploadIDMarker != last.UploadID ||
			(result.NextKeyMarker == keyMarker && result.NextUploadIDMarker == idMarker) {
			return nil, ErrConflict
		}
		keyMarker, idMarker = result.NextKeyMarker, result.NextUploadIDMarker
	}
	return nil, ErrConflict
}

func (s *COSObjects) abortPurgeUpload(ctx context.Context, owner string, t purgeTarget, id string) error {
	if !t.valid(owner) || !validCOSMultipartID(id) {
		return ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := s.client.Object.AbortMultipartUpload(ctx, t.Key, id)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		var provider *cos.ErrorResponse
		if response != nil && response.StatusCode == http.StatusNotFound && errors.As(err, &provider) && provider.Code == "NoSuchUpload" {
			return nil
		}
		return ErrCOSOperation
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		return ErrCOSOperation
	}
	return nil
}
