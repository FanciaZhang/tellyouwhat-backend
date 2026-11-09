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

func (h *uploadHTTP) collections(c *gin.Context) cloudCollectionRepository {
	if h.lifecycle(c, false) == nil {
		return nil
	}
	r, ok := h.service.repository.(cloudCollectionRepository)
	if !ok {
		writeLibraryError(c, ErrReadUnavailable)
		return nil
	}
	return r
}
func (h *uploadHTTP) GetAlbumCollection(c *gin.Context, id albumhttpapi.CollectionID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.Collection(c.Request.Context(), c.GetString("album.owner"), id, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *uploadHTTP) SaveAlbumCollection(c *gin.Context, id albumhttpapi.CollectionID) {
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	var input CloudCollectionInput
	if err := decodeCollectionFields(c, map[string]any{"requestID": &input.RequestID, "expectedRevision": &input.ExpectedRevision, "name": &input.Name, "deleted": &input.Deleted}); err != nil {
		writeLibraryError(c, err)
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.SaveCollection(c.Request.Context(), c.GetString("album.owner"), id, input, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *uploadHTTP) ListAlbumCollections(c *gin.Context, params albumhttpapi.ListAlbumCollectionsParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.Collections(c.Request.Context(), c.GetString("album.owner"), after, limit, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *uploadHTTP) GetAlbumCollectionMember(c *gin.Context, id albumhttpapi.CollectionID, asset albumhttpapi.AssetID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.CollectionMember(c.Request.Context(), c.GetString("album.owner"), id, asset, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *uploadHTTP) SaveAlbumCollectionMember(c *gin.Context, id albumhttpapi.CollectionID, asset albumhttpapi.AssetID) {
	if c.Request.URL.RawQuery != "" {
		writeLibraryError(c, ErrInvalidManifest)
		return
	}
	var input CloudCollectionMemberInput
	if err := decodeCollectionFields(c, map[string]any{"requestID": &input.RequestID, "expectedRevision": &input.ExpectedRevision, "present": &input.Present}); err != nil {
		writeLibraryError(c, err)
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.SaveCollectionMember(c.Request.Context(), c.GetString("album.owner"), id, asset, input, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *uploadHTTP) ListAlbumCollectionMembers(c *gin.Context, params albumhttpapi.ListAlbumCollectionMembersParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	r := h.collections(c)
	if r == nil {
		return
	}
	v, err := r.CollectionMembers(c.Request.Context(), c.GetString("album.owner"), after, limit, h.service.now().UTC())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func decodeCollectionFields(c *gin.Context, fields map[string]any) error {
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return ErrInvalidManifest
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 2048))
	if err != nil || !utf8.Valid(data) {
		return ErrInvalidManifest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if t, err := decoder.Token(); err != nil || t != json.Delim('{') {
		return ErrInvalidManifest
	}
	seen := map[string]bool{}
	for decoder.More() {
		t, err := decoder.Token()
		name, ok := t.(string)
		if err != nil || !ok || seen[name] || fields[name] == nil {
			return ErrInvalidManifest
		}
		seen[name] = true
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil || bytes.Equal(raw, []byte("null")) {
			return ErrInvalidManifest
		}
		if json.Unmarshal(raw, fields[name]) != nil {
			return ErrInvalidManifest
		}
	}
	if t, err := decoder.Token(); err != nil || t != json.Delim('}') || len(seen) != len(fields) {
		return ErrInvalidManifest
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrInvalidManifest
	}
	return nil
}
