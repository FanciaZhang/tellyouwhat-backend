package albums

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"strings"
	"time"
)

// VerifyStagingResource resolves an uncertain assembly by checking actual bytes,
// not ETags or client metadata. The GET is pinned to the version observed by HEAD.
// Absence/mismatch does not prove that a concurrent completion cannot still win.
func (s *COSObjects) VerifyStagingResource(ctx context.Context, key string, resource Resource) (string, error) {
	if !multipartStagingKey(key) || !strings.HasSuffix(key, "/"+resource.ID) {
		return "", ErrInvalidManifest
	}
	if _, err := NewMultipartLayout(resource.SizeBytes); err != nil {
		return "", err
	}
	expected, err := hex.DecodeString(resource.SHA256)
	if err != nil || len(expected) != sha256.Size || resource.SHA256 != strings.ToLower(resource.SHA256) {
		return "", ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	head, err := s.client.Object.Head(ctx, key, nil)
	if err != nil {
		return "", ErrCOSOperation
	}
	if head == nil || head.ContentLength != resource.SizeBytes {
		return "", ErrIncompleteBackup
	}
	version := head.Header.Get("x-cos-version-id")
	if !validMultipartVersion(version) {
		return "", ErrIncompleteBackup
	}
	response, err := s.client.Object.Get(ctx, key, nil, url.QueryEscape(version))
	if err != nil {
		return "", ErrCOSOperation
	}
	if response == nil || response.Body == nil {
		return "", ErrIncompleteBackup
	}
	defer response.Body.Close()
	if response.Header.Get("x-cos-version-id") != version || response.ContentLength != resource.SizeBytes {
		return "", ErrIncompleteBackup
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(response.Body, resource.SizeBytes+1))
	if err != nil {
		return "", ErrCOSOperation
	}
	if count != resource.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != resource.SHA256 {
		return "", ErrIncompleteBackup
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return version, nil
}
