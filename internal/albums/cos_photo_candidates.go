package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrPhotoCandidate = errors.New("album photo processing candidate unavailable")

type PhotoPreset string

const (
	PhotoGridJPEG  PhotoPreset = "grid_jpeg_v1"
	PhotoDailyJPEG PhotoPreset = "daily_jpeg_v1"
	PhotoGridPNG   PhotoPreset = "grid_png_v1"
	PhotoDailyPNG  PhotoPreset = "daily_png_v1"
)

// PhotoCandidate is untrusted for replacement purposes: decoding is necessary,
// but color/orientation, source selection, savings and system import still need
// verification. It is neither a published derivative nor backup/deletion proof.
type PhotoCandidate struct {
	Bytes         []byte
	Format        string
	Width, Height int
	SHA256        string
}

func photoPresetRule(preset PhotoPreset) (rule, format string, edge int, limit int64) {
	switch preset {
	case PhotoGridJPEG:
		return "imageMogr2/auto-orient/thumbnail/512x512>/format/jpg/quality/80", "jpeg", 512, 2 << 20
	case PhotoDailyJPEG:
		return "imageMogr2/auto-orient/thumbnail/2560x2560>/format/jpg/quality/85", "jpeg", 2560, 16 << 20
	case PhotoGridPNG:
		return "imageMogr2/auto-orient/thumbnail/512x512>/format/png", "png", 512, 4 << 20
	case PhotoDailyPNG:
		return "imageMogr2/auto-orient/thumbnail/2560x2560>/format/png", "png", 2560, 32 << 20
	default:
		return "", "", 0, 0
	}
}

// RenderPhotoCandidate is a bounded internal worker primitive, not a client
// endpoint. CI does the resize/encode; only the small result traverses the
// worker. Persist once before making it available to clients, rather than
// invoking processing on each gallery view. The caller must authorize the
// published source and select the current edited resource when appropriate.
func (s *COSObjects) RenderPhotoCandidate(ctx context.Context, source SealedObject, preset PhotoPreset) (PhotoCandidate, error) {
	rule, _, _, limit := photoPresetRule(preset)
	if rule == "" || !albumObjectKey(source.Key, "originals") || !strings.HasSuffix(source.Key, "/"+source.ResourceID) || !concreteCOSVersion(source.VersionID) || source.SizeBytes <= 0 {
		return PhotoCandidate{}, ErrInvalidManifest
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// CI.Get interpolates VersionId without escaping in SDK v0.7.75. Escape it
	// here so opaque versions containing &, +, # or = cannot change the query.
	response, err := s.client.CI.Get(ctx, source.Key, rule, nil, url.QueryEscape(source.VersionID))
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return PhotoCandidate{}, ErrCOSOperation
	}
	if response == nil || response.Body == nil {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("x-cos-version-id") != source.VersionID || response.Header.Get("Content-Range") != "" ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") || response.ContentLength > limit {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit || (response.ContentLength >= 0 && response.ContentLength != int64(len(data))) || ctx.Err() != nil {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	candidate, err := inspectPhotoCandidate(data, preset)
	if ctx.Err() != nil {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	return candidate, err
}

func inspectPhotoCandidate(data []byte, preset PhotoPreset) (PhotoCandidate, error) {
	_, format, edge, limit := photoPresetRule(preset)
	if format == "" || len(data) == 0 || int64(len(data)) > limit {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	config, actual, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || actual != format || config.Width <= 0 || config.Height <= 0 || config.Width > edge || config.Height > edge {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	// Decode the whole payload: a valid header alone does not prove that the
	// image body can be read. Dimensions above bound decompression allocations.
	decoded, actual, err := image.Decode(bytes.NewReader(data))
	if err != nil || actual != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return PhotoCandidate{}, ErrPhotoCandidate
	}
	digest := sha256.Sum256(data)
	return PhotoCandidate{Bytes: data, Format: format, Width: config.Width, Height: config.Height, SHA256: hex.EncodeToString(digest[:])}, nil
}
