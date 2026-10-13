package albums

import (
	"context"
	"strings"
	"time"
)

// FindStagingMultipart discovers a single unfinished upload for the exact,
// server-generated key. No match is not proof that initiation never succeeded.
// Ambiguity is preserved instead of picking or aborting an arbitrary session.
func (s *COSObjects) FindStagingMultipart(ctx context.Context, key string) (string, error) {
	if !albumObjectKey(key, "staging") {
		return "", ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keyMarker, idMarker := "", ""
	markers := map[string]bool{}
	seen := map[string]bool{}
	found := ""
	for page := 0; page < 10; page++ {
		result, err := s.readMultipartInventory(ctx, key, keyMarker, idMarker)
		if err != nil {
			return "", ErrCOSOperation
		}
		if result.Bucket != strings.Split(s.host, ".cos.")[0] || result.KeyMarker != keyMarker || result.UploadIDMarker != idMarker || result.IsTruncated == nil || result.MaxUploads != 1000 || result.Prefix != key || result.EncodingType != "" || result.Delimiter != "" || len(result.CommonPrefixes) != 0 || len(result.Uploads) > 1000 {
			return "", ErrMultipartPending
		}
		for _, upload := range result.Uploads {
			if !strings.HasPrefix(upload.Key, key) || !validCOSMultipartID(upload.UploadID) {
				return "", ErrMultipartPending
			}
			pair := upload.Key + "\x00" + upload.UploadID
			if seen[pair] {
				return "", ErrMultipartPending
			}
			seen[pair] = true
			if upload.Key == key {
				if found != "" {
					return "", ErrMultipartPending
				}
				found = upload.UploadID
			}
		}
		if !*result.IsTruncated {
			if found == "" {
				return "", ErrMultipartPending
			}
			return found, nil
		}
		if len(result.Uploads) == 0 {
			return "", ErrMultipartPending
		}
		last := result.Uploads[len(result.Uploads)-1]
		if result.NextKeyMarker != last.Key || result.NextUploadIDMarker != last.UploadID {
			return "", ErrMultipartPending
		}
		pair := result.NextKeyMarker + "\x00" + result.NextUploadIDMarker
		if markers[pair] {
			return "", ErrMultipartPending
		}
		markers[pair] = true
		keyMarker, idMarker = result.NextKeyMarker, result.NextUploadIDMarker
	}
	return "", ErrMultipartPending
}
