package albums

import (
	"context"
	"mime"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The attachment filename is authenticated together with the exact object
// version. It is descriptive metadata, never a filesystem path or object key.
func (s *COSObjects) AuthorizeSharedOriginalRead(ctx context.Context, object SealedObject, filename string, expiry time.Time) (string, error) {
	if !albumObjectKey(object.Key, "originals") || !strings.HasSuffix(object.Key, "/"+object.ResourceID) || object.SizeBytes <= 0 || !utf8.ValidString(filename) || len(filename) == 0 || len(filename) > 640 || strings.ContainsAny(filename, "/\\") {
		return "", ErrInvalidManifest
	}
	for _, r := range filename {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrInvalidManifest
		}
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		return "", ErrInvalidManifest
	}
	return s.signImmutableReadWithQuery(ctx, object.Key, object.VersionID, expiry, url.Values{"response-content-disposition": []string{disposition}})
}
