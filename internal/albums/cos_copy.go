package albums

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
	"golang.org/x/sync/errgroup"
)

var ErrCOSCleanup = errors.New("album multipart cleanup requires reconciliation")

// copyVersion keeps the upload ID until completion and aborts failed copies even
// when the caller's context has expired. Process death or an unavailable storage
// service still requires bucket lifecycle/reconciliation; abort is not a guarantee.
func (s *COSObjects) copyVersion(ctx context.Context, from, to, version string, size int64) (archived string, err error) {
	if size <= 0 {
		return "", ErrIncompleteBackup
	}
	source := s.host + "/" + from
	if size <= 5<<30 {
		result, response, err := s.client.Object.Copy(ctx, to, source, nil, version)
		if err != nil {
			return "", err
		}
		// COS returns the destination version in XML, not the COPY response header.
		if response == nil || response.StatusCode != http.StatusOK || result == nil || !concreteCOSVersion(result.VersionId) {
			return "", ErrIncompleteBackup
		}
		return result.VersionId, nil
	}
	partSize := int64(64 << 20)
	if minimum := (size-1)/10000 + 1; minimum > partSize {
		partSize = minimum
	}
	if partSize > 5<<30 {
		return "", ErrInvalidManifest
	}
	count := int((size-1)/partSize + 1)
	upload, _, err := s.client.Object.InitiateMultipartUpload(ctx, to, nil)
	if err != nil {
		return "", err
	}
	if upload == nil || upload.UploadID == "" {
		return "", ErrIncompleteBackup
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		// Cleanup has its own short budget; cancellation must not turn abort into
		// an immediate no-op. Wait for this attempt before allowing worker retry.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, abortErr := s.client.Object.AbortMultipartUpload(cleanup, to, upload.UploadID); abortErr != nil {
			err = errors.Join(err, ErrCOSCleanup)
		}
	}()
	parts := make([]cos.Object, count)
	group, copyContext := errgroup.WithContext(ctx)
	group.SetLimit(2)
	for index := 0; index < count; index++ {
		if copyContext.Err() != nil {
			break
		}
		group.Go(func() error {
			if err := copyContext.Err(); err != nil {
				return err
			}
			start := int64(index) * partSize
			end := start + min(partSize, size-start) - 1
			part, _, err := s.client.Object.CopyPart(copyContext, to, upload.UploadID, index+1, source, &cos.ObjectCopyPartOptions{XCosCopySourceRange: fmt.Sprintf("bytes=%d-%d", start, end)}, version)
			if err != nil {
				return err
			}
			if part == nil || part.ETag == "" {
				return ErrIncompleteBackup
			}
			parts[index] = cos.Object{PartNumber: index + 1, ETag: part.ETag}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, response, err := s.client.Object.CompleteMultipartUpload(ctx, to, upload.UploadID, &cos.CompleteMultipartUploadOptions{Parts: parts})
	if err != nil {
		return "", err
	}
	complete = true
	if response == nil || response.StatusCode != http.StatusOK || !concreteCOSVersion(response.Header.Get("x-cos-version-id")) {
		return "", ErrIncompleteBackup
	}
	return response.Header.Get("x-cos-version-id"), nil
}
