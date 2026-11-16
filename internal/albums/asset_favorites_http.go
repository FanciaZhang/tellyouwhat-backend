package albums

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func (h *uploadHTTP) favorites(c *gin.Context) assetFavoriteRepository {
	r, ok := h.service.repository.(assetFavoriteRepository)
	if !ok {
		writeLibraryError(c, ErrReadUnavailable)
		return nil
	}
	return r
}
func (h *uploadHTTP) GetAlbumAssetFavorite(c *gin.Context, assetID albumhttpapi.AssetID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	r := h.favorites(c)
	if r == nil {
		return
	}
	value, err := r.AssetFavorite(c.Request.Context(), c.GetString("album.owner"), assetID, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *uploadHTTP) SaveAlbumAssetFavorite(c *gin.Context, assetID albumhttpapi.AssetID) {
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	input, err := decodeAssetFavorite(c)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	r := h.favorites(c)
	if r == nil {
		return
	}
	value, err := r.SaveAssetFavorite(c.Request.Context(), c.GetString("album.owner"), assetID, input, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *uploadHTTP) ListAlbumAssetFavorites(c *gin.Context, params albumhttpapi.ListAlbumAssetFavoritesParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	r := h.favorites(c)
	if r == nil {
		return
	}
	value, err := r.AssetFavorites(c.Request.Context(), c.GetString("album.owner"), after, limit, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func decodeAssetFavorite(c *gin.Context) (AssetFavoriteInput, error) {
	var input AssetFavoriteInput
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return input, ErrInvalidManifest
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1024))
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
		case "favorite":
			err = json.Unmarshal(raw, &input.Favorite)
		default:
			return input, ErrInvalidManifest
		}
		if err != nil {
			return input, ErrInvalidManifest
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != 3 {
		return input, ErrInvalidManifest
	}
	if decoder.Decode(new(any)) != io.EOF {
		return input, ErrInvalidManifest
	}
	return input, nil
}
