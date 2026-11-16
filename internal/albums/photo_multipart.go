package albums

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

var errPhotoPartsPending = errors.New("album photo multipart needs bytes")

func (s *COSObjects) StartPhotoMultipart(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	key, err := spec.key()
	if err != nil || !spec.Multipart {
		return "", ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, format, _, _ := photoPresetRule(spec.Preset)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, response, err := s.client.Object.InitiateMultipartUpload(ctx, key, &cos.InitiateMultipartUploadOptions{
		ACLHeaderOptions:       &cos.ACLHeaderOptions{XCosACL: "private"},
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{ContentType: "image/" + format, CacheControl: "private, max-age=31536000, immutable"},
	})
	closePhotoResponse(response)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || v == nil || v.Key != key || !validCOSMultipartID(v.UploadID) {
		return "", ErrPhotoWriteUncertain
	}
	return v.UploadID, nil
}

func (s *COSObjects) FindPhotoMultipart(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	key, err := spec.key()
	if err != nil || !spec.Multipart {
		return "", ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.findMultipart(ctx, key)
}

// The repository must bind the provider ID before this method receives bytes.
// Reusing part 1 in the same session cannot create another object version.
func (s *COSObjects) StorePhotoMultipartOnce(ctx context.Context, spec PhotoWriteSpec, id string, candidate PhotoCandidate, beforeComplete func(context.Context) error) (SealedObject, error) {
	key, err := spec.key()
	if err != nil || !spec.Multipart || !validCOSMultipartID(id) || beforeComplete == nil {
		return SealedObject{}, ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	checked, err := inspectPhotoCandidate(candidate.Bytes, spec.Preset)
	if err != nil || checked.SHA256 != spec.SHA256 || int64(len(checked.Bytes)) != spec.SizeBytes || checked.Width != spec.Width || checked.Height != spec.Height ||
		candidate.SHA256 != checked.SHA256 || candidate.Width != checked.Width || candidate.Height != checked.Height || candidate.Format != checked.Format {
		return SealedObject{}, ErrPhotoCandidate
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	checksum := md5.Sum(candidate.Bytes)
	response, err := s.client.Object.UploadPart(ctx, key, id, 1, bytes.NewReader(candidate.Bytes), &cos.ObjectUploadPartOptions{
		ContentLength: spec.SizeBytes, ContentMD5: base64.StdEncoding.EncodeToString(checksum[:]),
	})
	closePhotoResponse(response)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return s.completePhotoMultipart(ctx, spec, key, id, beforeComplete)
}

// A lost completion response is reconciled against the exact key before the
// existing session is touched; a missing object never permits re-initiation.
func (s *COSObjects) ReconcilePhotoMultipart(ctx context.Context, spec PhotoWriteSpec, id string, beforeComplete func(context.Context) error) (SealedObject, error) {
	key, err := spec.key()
	if err != nil || !spec.Multipart || !validCOSMultipartID(id) || beforeComplete == nil {
		return SealedObject{}, ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := s.client.Object.Head(ctx, key, nil)
	closePhotoResponse(response)
	if err == nil && response != nil && response.StatusCode == http.StatusOK {
		if response.ContentLength != spec.SizeBytes || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
			return SealedObject{}, ErrPhotoWriteUncertain
		}
		return s.verifyPhotoWrite(ctx, spec, key, response.Header.Get("x-cos-version-id"))
	}
	if response == nil || response.StatusCode != http.StatusNotFound {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return s.completePhotoMultipart(ctx, spec, key, id, beforeComplete)
}

func (s *COSObjects) completePhotoMultipart(ctx context.Context, spec PhotoWriteSpec, key, id string, beforeComplete func(context.Context) error) (SealedObject, error) {
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	parts, response, err := s.client.Object.ListParts(ctx, key, id, &cos.ObjectListPartsOptions{MaxParts: "2"})
	closePhotoResponse(response)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || parts == nil || parts.Key != key || parts.UploadID != id || parts.IsTruncated {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	if len(parts.Parts) == 0 {
		return SealedObject{}, errPhotoPartsPending
	}
	if len(parts.Parts) != 1 || parts.Parts[0].PartNumber != 1 || parts.Parts[0].Size != spec.SizeBytes || parts.Parts[0].ETag == "" || len(parts.Parts[0].ETag) > 256 || strings.ContainsAny(parts.Parts[0].ETag, "\r\n") {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	if err := beforeComplete(ctx); err != nil {
		return SealedObject{}, err
	}
	if err := ctx.Err(); err != nil {
		return SealedObject{}, err
	}
	v, response, err := s.client.Object.CompleteMultipartUpload(ctx, key, id, &cos.CompleteMultipartUploadOptions{Parts: []cos.Object{{PartNumber: 1, ETag: parts.Parts[0].ETag}}})
	closePhotoResponse(response)
	if err != nil || response == nil || response.StatusCode != http.StatusOK || v == nil || v.Key != key || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return s.verifyPhotoWrite(ctx, spec, key, response.Header.Get("x-cos-version-id"))
}

func closePhotoResponse(response *cos.Response) {
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
}
