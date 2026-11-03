package albums

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

var ErrCOSMultipartUnknown = errors.New("album multipart outcome requires reconciliation")

// Geometry is derived from the verified manifest size, never client ranges.
type MultipartLayout struct {
	SizeBytes, PartSize int64
	PartCount           int
}

func NewMultipartLayout(size int64) (MultipartLayout, error) {
	if size <= 0 {
		return MultipartLayout{}, ErrInvalidManifest
	}
	partSize := int64(8 << 20)
	minimum := (size-1)/10000 + 1
	if minimum > partSize {
		partSize = ((minimum-1)/(1<<20) + 1) * (1 << 20)
	}
	if partSize > 5<<30 {
		return MultipartLayout{}, ErrInvalidManifest
	}
	return MultipartLayout{size, partSize, int((size-1)/partSize + 1)}, nil
}
func (l MultipartLayout) Part(number int) (offset, size int64, err error) {
	canonical, err := NewMultipartLayout(l.SizeBytes)
	if err != nil || canonical != l || number < 1 || number > l.PartCount {
		return 0, 0, ErrInvalidManifest
	}
	offset = int64(number-1) * l.PartSize
	return offset, min(l.PartSize, l.SizeBytes-offset), nil
}

type MultipartPartGrant struct {
	PutGrant
	PartNumber int   `json:"partNumber"`
	Offset     int64 `json:"offset"`
	SizeBytes  int64 `json:"sizeBytes"`
}

var cosMultipartIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validCOSMultipartID(id string) bool {
	return len(id) <= 1024 && cosMultipartIDPattern.MatchString(id)
}

func multipartStagingKey(key string) bool {
	if albumObjectKey(key, "staging") {
		return true
	}
	p := strings.Split(key, "/")
	return len(p) == 6 && p[0] == "albums" && p[1] == "staging" && p[2] == "live-pairs" && validOwner(p[3]) && validOwner(p[4]) && (p[5] == "photo" || p[5] == "motion")
}
func validMultipartTarget(key, id string) bool {
	return multipartStagingKey(key) && validCOSMultipartID(id)
}

// InitiateStagingMultipart returns a provider ID that the service must durably
// bind to the account/upload/resource before issuing any client part grants.
// Unknown outcomes need reconciliation/lifecycle cleanup, not blind initiation.
func (s *COSObjects) InitiateStagingMultipart(ctx context.Context, key string) (string, error) {
	if !multipartStagingKey(key) {
		return "", ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	value, _, err := s.client.Object.InitiateMultipartUpload(ctx, key, nil)
	if err != nil {
		return "", errors.Join(ErrCOSOperation, ErrCOSMultipartUnknown)
	}
	if value == nil || value.Key != key || !validCOSMultipartID(value.UploadID) {
		return "", errors.Join(ErrIncompleteBackup, ErrCOSMultipartUnknown)
	}
	return value.UploadID, nil
}
func (s *COSObjects) AuthorizePart(ctx context.Context, key, uploadID string, resource Resource, number int, expiry time.Time) (MultipartPartGrant, error) {
	layout, err := NewMultipartLayout(resource.SizeBytes)
	if err != nil {
		return MultipartPartGrant{}, err
	}
	offset, size, err := layout.Part(number)
	now := s.now()
	duration := expiry.Sub(now)
	if err != nil || !validMultipartTarget(key, uploadID) || !strings.HasSuffix(key, "/"+resource.ID) || duration <= 0 || duration > 10*time.Minute {
		return MultipartPartGrant{}, ErrInvalidManifest
	}
	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(size, 10))
	headers.Set("Content-Type", "application/octet-stream")
	query := url.Values{"uploadId": {uploadID}, "partNumber": {strconv.Itoa(number)}}
	signed, err := s.uploadClient.Object.GetPresignedURL2(ctx, http.MethodPut, key, duration, &cos.PresignedURLOptions{Header: &headers, Query: &query, AuthTime: &cos.AuthTime{SignStartTime: now, SignEndTime: expiry, KeyStartTime: now, KeyEndTime: expiry}})
	if err != nil {
		return MultipartPartGrant{}, ErrCOSOperation
	}
	return MultipartPartGrant{PutGrant: PutGrant{ResourceID: resource.ID, URL: signed.String(), Headers: map[string]string{"Content-Length": strconv.FormatInt(size, 10), "Content-Type": "application/octet-stream"}, ExpiresAt: expiry}, PartNumber: number, Offset: offset, SizeBytes: size}, nil
}

// Completion uses COS's own listing, not caller ETags. This only assembles a
// staging object: exact-version reading and SHA-256 verification remain required.
func (s *COSObjects) CompleteStagingMultipart(ctx context.Context, key, uploadID string, size int64) (string, error) {
	layout, err := NewMultipartLayout(size)
	if err != nil || !validMultipartTarget(key, uploadID) {
		return "", ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	parts, err := s.listStagingMultipartParts(ctx, key, uploadID, layout)
	if err != nil {
		return "", err
	}
	if len(parts) != layout.PartCount {
		return "", ErrIncompleteBackup
	}
	for i := range parts {
		parts[i] = cos.Object{PartNumber: parts[i].PartNumber, ETag: parts[i].ETag}
	}
	_, response, err := s.client.Object.CompleteMultipartUpload(ctx, key, uploadID, &cos.CompleteMultipartUploadOptions{Parts: parts})
	if err != nil {
		return "", errors.Join(ErrCOSOperation, ErrCOSMultipartUnknown)
	}
	if response == nil || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return "", errors.Join(ErrIncompleteBackup, ErrCOSMultipartUnknown)
	}
	return response.Header.Get("x-cos-version-id"), nil
}

// MultipartUploadedPart is a provider observation for resuming byte transfer,
// never backup evidence. A successful full-resource hash is still required.
type MultipartUploadedPart struct {
	PartNumber int   `json:"partNumber"`
	SizeBytes  int64 `json:"sizeBytes"`
}

func (s *COSObjects) ListStagingMultipartParts(ctx context.Context, key, uploadID string, size int64) ([]MultipartUploadedPart, error) {
	layout, err := NewMultipartLayout(size)
	if err != nil || !validMultipartTarget(key, uploadID) {
		return nil, ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	parts, err := s.listStagingMultipartParts(ctx, key, uploadID, layout)
	if err != nil {
		return nil, err
	}
	result := make([]MultipartUploadedPart, 0, len(parts))
	for _, part := range parts {
		result = append(result, MultipartUploadedPart{part.PartNumber, part.Size})
	}
	return result, nil
}

// Sparse, ordered lists are valid during upload. Every observed part must still
// have the manifest-derived length; duplicates and nonadvancing pages fail.
func (s *COSObjects) listStagingMultipartParts(ctx context.Context, key, uploadID string, layout MultipartLayout) ([]cos.Object, error) {
	parts := make([]cos.Object, 0, layout.PartCount)
	marker := 0
	for {
		result, _, err := s.client.Object.ListParts(ctx, key, uploadID, &cos.ObjectListPartsOptions{MaxParts: "1000", PartNumberMarker: strconv.Itoa(marker)})
		if err != nil {
			return nil, ErrCOSOperation
		}
		if result == nil || result.Key != key || result.UploadID != uploadID || len(result.Parts) > 1000 {
			return nil, ErrIncompleteBackup
		}
		for _, part := range result.Parts {
			_, expected, err := layout.Part(part.PartNumber)
			if err != nil || part.PartNumber <= marker || part.Size != expected || part.ETag == "" || len(part.ETag) > 256 || strings.ContainsAny(part.ETag, "\r\n") {
				return nil, ErrIncompleteBackup
			}
			parts = append(parts, cos.Object{PartNumber: part.PartNumber, ETag: part.ETag, Size: part.Size})
			marker = part.PartNumber
		}
		if !result.IsTruncated {
			break
		}
		next, err := strconv.Atoi(result.NextPartNumberMarker)
		if err != nil || len(result.Parts) == 0 || next != marker || next >= layout.PartCount {
			return nil, ErrIncompleteBackup
		}
	}
	return parts, nil
}

func (s *COSObjects) AbortStagingMultipart(ctx context.Context, key, uploadID string) error {
	if !validMultipartTarget(key, uploadID) {
		return ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.client.Object.AbortMultipartUpload(ctx, key, uploadID); err != nil {
		return ErrCOSCleanup
	}
	return nil
}
