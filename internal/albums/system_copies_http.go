package albums

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *uploadHTTP) systemCopies(c *gin.Context) systemCopyRepository {
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return nil
	}
	repository, ok := h.service.repository.(systemCopyRepository)
	if !ok {
		writeLibraryError(c, ErrReadUnavailable)
		return nil
	}
	return repository
}
func (h *uploadHTTP) ListAlbumSystemCopies(c *gin.Context, versionID uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	repository := h.systemCopies(c)
	if repository == nil {
		return
	}
	result, err := repository.SystemCopies(c.Request.Context(), c.GetString("album.owner"), versionID.String(), h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) RememberAlbumSystemCopy(c *gin.Context, versionID uuid.UUID) {
	repository := h.systemCopies(c)
	if repository == nil {
		return
	}
	input, err := decodeSystemCopy(c)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	result, err := repository.RememberSystemCopy(c.Request.Context(), c.GetString("album.owner"), versionID.String(), input, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) ForgetAlbumSystemCopy(c *gin.Context, versionID uuid.UUID, locatorID string) {
	if !emptyAlbumBody(c) {
		return
	}
	repository := h.systemCopies(c)
	if repository == nil {
		return
	}
	if err := repository.ForgetSystemCopy(c.Request.Context(), c.GetString("album.owner"), versionID.String(), locatorID, h.service.now().UTC()); err != nil {
		writeLibraryError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func decodeSystemCopy(c *gin.Context) (SystemCopyInput, error) {
	var input SystemCopyInput
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return input, ErrInvalidManifest
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 32768))
	if err != nil || !utf8.Valid(data) {
		return input, ErrInvalidManifest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return input, ErrInvalidManifest
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return input, ErrInvalidManifest
		}
		seen[name] = true
		var value string
		if err := decoder.Decode(&value); err != nil {
			return input, ErrInvalidManifest
		}
		switch name {
		case "manifestDigest":
			input.ManifestDigest = value
		case "kind":
			input.Kind = value
		case "systemCloudIdentifier":
			input.SystemCloudIdentifier = value
		default:
			return input, ErrInvalidManifest
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return input, ErrInvalidManifest
	}
	if err := decoder.Decode(new(any)); err != io.EOF || len(seen) != 3 {
		return input, ErrInvalidManifest
	}
	return input, nil
}
