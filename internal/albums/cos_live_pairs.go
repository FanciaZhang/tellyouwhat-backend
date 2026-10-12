package albums

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

func livePairContentType(part string) string {
	if part == "photo" {
		return "image/jpeg"
	}
	return "video/quicktime"
}
func (s *COSObjects) AuthorizeLivePairPut(ctx context.Context, u LivePairUpload, part string, expiry time.Time) (PutGrant, error) {
	key, err := u.key(part, true)
	resource, resourceErr := u.resource(part)
	now := s.now()
	ttl := expiry.Sub(now)
	if err != nil || resourceErr != nil || u.Descriptor.validate() != nil || u.State != "uploading" || ttl <= 0 || ttl > 10*time.Minute || expiry.After(u.ExpiresAt) {
		return PutGrant{}, ErrInvalidManifest
	}
	headers := http.Header{}
	headers.Set("Content-Type", livePairContentType(part))
	headers.Set("Content-Length", strconv.FormatInt(resource.SizeBytes, 10))
	signed, err := s.uploadClient.Object.GetPresignedURL2(ctx, http.MethodPut, key, ttl, &cos.PresignedURLOptions{
		Header:   &headers,
		AuthTime: &cos.AuthTime{SignStartTime: now, SignEndTime: expiry, KeyStartTime: now, KeyEndTime: expiry},
	})
	if err != nil {
		return PutGrant{}, ErrCOSOperation
	}
	return PutGrant{ResourceID: part, URL: signed.String(), Headers: map[string]string{"Content-Type": livePairContentType(part), "Content-Length": strconv.FormatInt(resource.SizeBytes, 10)}, ExpiresAt: expiry}, nil
}

// Caller has committed exactly one consumed COPY intent. No archive PUT/COPY
// URL is ever given to a client, and the SDK is configured for one attempt.
func (s *COSObjects) SealLivePairOnce(ctx context.Context, u LivePairUpload, part string) (string, error) {
	from, err := u.key(part, true)
	to, targetErr := u.key(part, false)
	expected, resourceErr := u.resource(part)
	consumed := (part == "photo" && u.PhotoConsumed) || (part == "motion" && u.MovieConsumed)
	if err != nil || targetErr != nil || resourceErr != nil || !consumed || u.Descriptor.validate() != nil || u.State != "verifying" {
		return "", ErrInvalidManifest
	}
	response, err := s.client.Object.Head(ctx, from, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK || response.ContentLength != expected.SizeBytes || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return "", ErrLivePairNotReady
	}
	archived, err := s.copyVersion(ctx, from, to, response.Header.Get("x-cos-version-id"), expected.SizeBytes)
	if err != nil || !concreteCOSVersion(archived) {
		return "", ErrLivePairNotReady
	}
	return archived, nil
}

// A missing HEAD is uncertainty, never permission for another COPY. This also
// covers process death between COPY success and the durable receipt commit.
func (s *COSObjects) ReconcileLivePairSeal(ctx context.Context, u LivePairUpload, part string) (string, error) {
	key, err := u.key(part, false)
	expected, resourceErr := u.resource(part)
	if err != nil || resourceErr != nil || u.Descriptor.validate() != nil || u.State != "verifying" ||
		(part == "photo" && !u.PhotoConsumed) || (part == "motion" && !u.MovieConsumed) {
		return "", ErrInvalidManifest
	}
	response, err := s.client.Object.Head(ctx, key, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK || response.ContentLength != expected.SizeBytes || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return "", ErrLivePairNotReady
	}
	return response.Header.Get("x-cos-version-id"), nil
}
func (s *COSObjects) OpenLivePairVersion(ctx context.Context, u LivePairUpload, part, version string) (io.ReadCloser, error) {
	key, err := u.key(part, false)
	expected, resourceErr := u.resource(part)
	if err != nil || resourceErr != nil || !concreteCOSVersion(version) || u.Descriptor.validate() != nil {
		return nil, ErrInvalidManifest
	}
	response, err := s.client.Object.Get(ctx, key, nil, url.QueryEscape(version))
	if err != nil || response == nil || response.Body == nil || response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != version ||
		response.ContentLength != expected.SizeBytes || response.Header.Get("Content-Range") != "" || response.Uncompressed ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, ErrIncompleteBackup
	}
	return response.Body, nil
}
func (s *COSObjects) AuthorizeLivePairRead(ctx context.Context, u LivePairUpload, object SealedObject, expiry time.Time) (string, error) {
	if u.State != "verified" || u.validateReceipt(object) != nil {
		return "", ErrInvalidManifest
	}
	return s.signImmutableRead(ctx, object.Key, object.VersionID, expiry)
}

var _ LivePairStorage = (*COSObjects)(nil)
