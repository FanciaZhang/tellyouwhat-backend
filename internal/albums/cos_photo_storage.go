package albums

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

var ErrPhotoWriteUncertain = errors.New("album photo write needs reconciliation")

// PhotoWriteSpec must be durably recorded before starting a PUT. AttemptID is
// unique to ONE PUT attempt, not a reusable worker lease or source version ID.
// Persisting this small descriptor allows recovery without regenerating images.
// It identifies a candidate only; publishing also needs source/manifest binding
// and transactional quota accounting in the durable task repository.
type PhotoWriteSpec struct {
	OwnerID   string      `json:"ownerID"`
	AttemptID string      `json:"attemptID"`
	Preset    PhotoPreset `json:"preset"`
	SizeBytes int64       `json:"sizeBytes"`
	SHA256    string      `json:"sha256"`
	Width     int         `json:"width"`
	Height    int         `json:"height"`
}

func (spec PhotoWriteSpec) key() (string, error) {
	_, _, edge, limit := photoPresetRule(spec.Preset)
	hash, err := hex.DecodeString(spec.SHA256)
	if !validOwner(spec.OwnerID) || !validOwner(spec.AttemptID) || limit == 0 || spec.SizeBytes <= 0 || spec.SizeBytes > limit ||
		spec.Width <= 0 || spec.Height <= 0 || spec.Width > edge || spec.Height > edge || err != nil || len(hash) != 32 || strings.ToLower(spec.SHA256) != spec.SHA256 {
		return "", ErrInvalidManifest
	}
	return "albums/derivatives/" + spec.OwnerID + "/" + spec.AttemptID + "/" + string(spec.Preset), nil
}

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

// ReconcilePhotoCandidate performs only HEAD/GET, never PUT or DELETE. A 404
// cannot prove an earlier timed-out PUT will not commit later. Keep the durable
// attempt uncertain on failure; do not blindly rerender or release its budget.
func (s *COSObjects) ReconcilePhotoCandidate(ctx context.Context, spec PhotoWriteSpec) (SealedObject, error) {
	key, err := spec.key()
	if err != nil {
		return SealedObject{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	response, err := s.client.Object.Head(ctx, key, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK || response.ContentLength != spec.SizeBytes || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return s.verifyPhotoWrite(ctx, spec, key, response.Header.Get("x-cos-version-id"))
}

func (s *COSObjects) verifyPhotoWrite(ctx context.Context, spec PhotoWriteSpec, key, version string) (SealedObject, error) {
	response, err := s.client.Object.Get(ctx, key, nil, url.QueryEscape(version))
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != version ||
		response.ContentLength != spec.SizeBytes || response.Header.Get("Content-Range") != "" || response.Uncompressed ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, spec.SizeBytes+1))
	if err != nil || int64(len(data)) != spec.SizeBytes || ctx.Err() != nil {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	checked, err := inspectPhotoCandidate(data, spec.Preset)
	if err != nil || checked.SHA256 != spec.SHA256 || checked.Width != spec.Width || checked.Height != spec.Height || ctx.Err() != nil {
		return SealedObject{}, ErrPhotoWriteUncertain
	}
	return SealedObject{ResourceID: string(spec.Preset), Key: key, VersionID: version, SizeBytes: spec.SizeBytes, SHA256: spec.SHA256}, nil
}

// A committed version is sufficient to observe a finished PUT for deletion,
// including an invalid preview. No bytes are read or published by this query.
func (s *COSObjects) observePhotoDeletion(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	key, err := spec.key()
	if err != nil {
		return "", err
	}
	response, err := s.client.Object.Head(ctx, key, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK || response.ContentLength < 0 || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return "", ErrPhotoWriteUncertain
	}
	return response.Header.Get("x-cos-version-id"), nil
}
