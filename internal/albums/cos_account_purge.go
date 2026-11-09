package albums

import (
	"context"
	"net/url"
	"strings"
)

var accountPurgeAreas = [...]string{"staging", "originals", "derivatives", "video-previews", "live-pairs", "staging/live-pairs"}

func accountPurgePrefix(owner, area string) (string, error) {
	if !validOwner(owner) {
		return "", ErrInvalidManifest
	}
	for _, allowed := range accountPurgeAreas {
		if area == allowed {
			return "albums/" + area + "/" + owner + "/", nil
		}
	}
	return "", ErrInvalidManifest
}

type accountPurgeVersion struct {
	Target  purgeTarget          `json:"target"`
	Version DerivedObjectVersion `json:"version"`
}

type accountPurgeUpload struct {
	Target   purgeTarget `json:"target"`
	UploadID string      `json:"uploadID"`
}

// Read the first bounded batch repeatedly as acknowledged entries are removed.
// This includes orphaned uploads with no published catalogue entry. Only a fresh,
// non-truncated empty response proves this prefix empty at observation time.
// Neither inventory nor deletion of a batch proves provider writes have stopped.
func (s *COSObjects) listAccountPurgeVersions(ctx context.Context, owner, area string) ([]accountPurgeVersion, error) {
	prefix, err := accountPurgePrefix(owner, area)
	if err != nil {
		return nil, err
	}
	var r cosVersionInventory
	if err := s.readCOSInventory(ctx, url.Values{"versions": {""}, "prefix": {prefix}, "max-keys": {"1000"}}, &r); err != nil {
		return nil, err
	}
	if r.Name != strings.Split(s.host, ".cos.")[0] || r.Prefix != prefix || r.EncodingType != "" || r.Delimiter != "" || len(r.CommonPrefixes) != 0 ||
		r.KeyMarker != "" || r.VersionIdMarker != "" || r.MaxKeys != 1000 || r.IsTruncated == nil || len(r.Version)+len(r.DeleteMarker) > 1000 {
		return nil, ErrConflict
	}
	result := make([]accountPurgeVersion, 0, len(r.Version)+len(r.DeleteMarker))
	seen := map[string]bool{}
	keys := map[string]bool{}
	add := func(key, id string, size int64, deleted bool) error {
		target := purgeTarget{key}
		identity := key + "\x00" + id
		if !strings.HasPrefix(key, prefix) || !target.valid(owner) || !concreteCOSVersion(id) || size < 0 || seen[identity] {
			return ErrConflict
		}
		seen[identity] = true
		keys[key] = true
		result = append(result, accountPurgeVersion{target, DerivedObjectVersion{id, deleted, size}})
		return nil
	}
	for _, v := range r.Version {
		if err := add(v.Key, v.VersionId, v.Size, false); err != nil {
			return nil, err
		}
	}
	for _, v := range r.DeleteMarker {
		if err := add(v.Key, v.VersionId, 0, true); err != nil {
			return nil, err
		}
	}
	if *r.IsTruncated {
		// COS may return a key-only continuation at a key boundary.
		if len(result) == 0 || !keys[r.NextKeyMarker] || r.NextVersionIdMarker != "" && !seen[r.NextKeyMarker+"\x00"+r.NextVersionIdMarker] {
			return nil, ErrConflict
		}
	} else if r.NextKeyMarker != "" || r.NextVersionIdMarker != "" {
		return nil, ErrConflict
	}
	return result, nil
}

func (s *COSObjects) listAccountPurgeUploads(ctx context.Context, owner, area string) ([]accountPurgeUpload, error) {
	prefix, err := accountPurgePrefix(owner, area)
	if err != nil {
		return nil, err
	}
	r, err := s.readMultipartInventory(ctx, prefix, "", "")
	if err != nil {
		return nil, err
	}
	if r.Bucket != strings.Split(s.host, ".cos.")[0] || r.Prefix != prefix || r.EncodingType != "" || r.Delimiter != "" || len(r.CommonPrefixes) != 0 ||
		r.KeyMarker != "" || r.UploadIDMarker != "" || r.MaxUploads != 1000 || r.IsTruncated == nil || len(r.Uploads) > 1000 {
		return nil, ErrConflict
	}
	result := make([]accountPurgeUpload, 0, len(r.Uploads))
	seen := map[string]bool{}
	for _, u := range r.Uploads {
		target := purgeTarget{u.Key}
		identity := u.Key + "\x00" + u.UploadID
		if !strings.HasPrefix(u.Key, prefix) || !target.valid(owner) || !validCOSMultipartID(u.UploadID) || seen[identity] {
			return nil, ErrConflict
		}
		seen[identity] = true
		result = append(result, accountPurgeUpload{target, u.UploadID})
	}
	if *r.IsTruncated {
		if len(result) == 0 || !seen[r.NextKeyMarker+"\x00"+r.NextUploadIDMarker] {
			return nil, ErrConflict
		}
	} else if r.NextKeyMarker != "" || r.NextUploadIDMarker != "" {
		return nil, ErrConflict
	}
	return result, nil
}
