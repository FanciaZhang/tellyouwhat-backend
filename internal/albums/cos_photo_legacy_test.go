package albums

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	cos "github.com/tencentyun/cos-go-sdk-v5"
	"net/http"
	"time"
)

// Historic direct-PUT fixtures exercise recovery; production writes use multipart sessions.
// StorePhotoCandidateOnce does exactly one PUT, then reads that exact provider
// version back. The caller must mark this attempt consumed BEFORE calling it.
// After any error, only ReconcilePhotoCandidate is safe for this AttemptID:
// versioned COS buckets ignore forbid-overwrite, and retrying a PUT can create
// another billable version even if the previous response was merely lost.
func (s *COSObjects) StorePhotoCandidateOnce(ctx context.Context, spec PhotoWriteSpec, candidate PhotoCandidate) (SealedObject, error) {
	key, err := spec.key()
	if err != nil {
		return SealedObject{}, err
	}
	checked, err := inspectPhotoCandidate(candidate.Bytes, spec.Preset)
	if err != nil || int64(len(checked.Bytes)) != spec.SizeBytes || checked.SHA256 != spec.SHA256 || checked.Width != spec.Width || checked.Height != spec.Height ||
		candidate.SHA256 != checked.SHA256 || candidate.Width != checked.Width || candidate.Height != checked.Height || candidate.Format != checked.Format {
		return SealedObject{}, ErrPhotoCandidate
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	checksum := md5.Sum(candidate.Bytes) // Transport checksum; SHA-256 is verified after GET.
	response, err := s.client.Object.Put(ctx, key, bytes.NewReader(candidate.Bytes), &cos.ObjectPutOptions{
		ACLHeaderOptions:       &cos.ACLHeaderOptions{XCosACL: "private"},
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "image/" + checked.Format, ContentLength: spec.SizeBytes, ContentMD5: base64.StdEncoding.EncodeToString(checksum[:]), CacheControl: "private, max-age=31536000, immutable"},
	})
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return s.verifyPhotoWrite(ctx, spec, key, response.Header.Get("x-cos-version-id"))
}
