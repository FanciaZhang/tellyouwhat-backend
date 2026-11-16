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
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func (h *uploadHTTP) contentCorrections(c *gin.Context) contentCorrectionRepository {
	repository, ok := h.service.repository.(contentCorrectionRepository)
	if !ok {
		writeLibraryError(c, ErrReadUnavailable)
		return nil
	}
	return repository
}
func (h *uploadHTTP) ListAlbumContentCorrections(c *gin.Context, params albumhttpapi.ListAlbumContentCorrectionsParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	repository := h.contentCorrections(c)
	if repository == nil {
		return
	}
	result, err := repository.ContentCorrections(c.Request.Context(), c.GetString("album.owner"), after, limit, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) GetAlbumContentCorrection(c *gin.Context, versionID uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	repository := h.contentCorrections(c)
	if repository == nil {
		return
	}
	result, err := repository.ContentCorrection(c.Request.Context(), c.GetString("album.owner"), versionID.String(), h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) SaveAlbumContentCorrection(c *gin.Context, versionID uuid.UUID) {
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	input, err := decodeContentCorrection(c)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	repository := h.contentCorrections(c)
	if repository == nil {
		return
	}
	result, err := repository.SaveContentCorrection(c.Request.Context(), c.GetString("album.owner"), versionID.String(), input, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func decodeContentCorrection(c *gin.Context) (ContentCorrectionInput, error) {
	var input ContentCorrectionInput
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return input, ErrInvalidManifest
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
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
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil || bytes.Equal(raw, []byte("null")) {
			return input, ErrInvalidManifest
		}
		switch name {
		case "requestID":
			err = json.Unmarshal(raw, &input.RequestID)
		case "expectedRevision":
			err = json.Unmarshal(raw, &input.ExpectedRevision)
		case "manifestDigest":
			err = json.Unmarshal(raw, &input.ManifestDigest)
		case "included":
			err = json.Unmarshal(raw, &input.Included)
		case "excluded":
			err = json.Unmarshal(raw, &input.Excluded)
		default:
			return input, ErrInvalidManifest
		}
		if err != nil {
			return input, ErrInvalidManifest
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != 5 {
		return input, ErrInvalidManifest
	}
	if decoder.Decode(new(any)) != io.EOF {
		return input, ErrInvalidManifest
	}
	return input, nil
}
