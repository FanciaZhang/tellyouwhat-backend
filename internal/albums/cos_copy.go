package albums

import (
	"context"
	"errors"
	"fmt"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
	"golang.org/x/sync/errgroup"
)

var ErrCOSCleanup = errors.New("album multipart cleanup requires reconciliation")

// copyVersion keeps the upload ID until completion and aborts failed copies even
// when the caller's context has expired. Process death or an unavailable storage
// service still requires bucket lifecycle/reconciliation; abort is not a guarantee.
func (s *COSObjects) copyVersion(ctx context.Context, from, to, version string, size int64) (response *cos.Response, err error) {
	if size <= 0 {
		return nil, ErrIncompleteBackup
	}
	source := s.host + "/" + from
	if size <= 5<<30 {
		_, response, err = s.client.Object.Copy(ctx, to, source, nil, version)
		return response, err
	}
	partSize := int64(64 << 20)
	if minimum := (size-1)/10000 + 1; minimum > partSize {
		partSize = minimum
	}
	if partSize > 5<<30 {
		return nil, ErrInvalidManifest
	}
	count := int((size-1)/partSize + 1)
	upload, _, err := s.client.Object.InitiateMultipartUpload(ctx, to, nil)
	if err != nil {
		return nil, err
	}
	if upload == nil || upload.UploadID == "" {
		return nil, ErrIncompleteBackup
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
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, response, err = s.client.Object.CompleteMultipartUpload(ctx, to, upload.UploadID, &cos.CompleteMultipartUploadOptions{Parts: parts})
	if err != nil {
		return nil, err
	}
	complete = true
	return response, nil
}
