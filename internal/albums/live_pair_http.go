package albums

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func writeLivePairError(c *gin.Context, err error) {
	if errors.Is(err, ErrLivePairNotReady) || errors.Is(err, ErrLivePhotoNotReady) {
		c.JSON(http.StatusConflict, gin.H{"code": "live_pair_not_ready"})
		return
	}
	writeLibraryError(c, err)
}
func (h *uploadHTTP) GetAlbumLivePair(c *gin.Context, version uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	u, err := h.service.LivePair(c.Request.Context(), c.GetString("album.owner"), version.String())
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, u)
}
func (h *uploadHTTP) CreateAlbumLivePair(c *gin.Context, version uuid.UUID) {
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	var request struct {
		RequestID  string             `json:"requestID"`
		Descriptor LivePairDescriptor `json:"descriptor"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16384)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&request) != nil || decoder.Decode(&extra) != io.EOF {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	u, err := h.service.CreateLivePair(c.Request.Context(), c.GetString("album.owner"), version.String(), request.RequestID, request.Descriptor)
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusCreated, u)
}
func (h *uploadHTTP) AuthorizeAlbumLivePairUpload(c *gin.Context, id uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grants, err := h.service.LivePairGrants(c.Request.Context(), c.GetString("album.owner"), id.String())
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"grants": grants})
}
func (h *uploadHTTP) SubmitAlbumLivePair(c *gin.Context, id uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	u, err := h.service.SubmitLivePair(c.Request.Context(), c.GetString("album.owner"), id.String())
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, u)
}
func (h *uploadHTTP) AuthorizeAlbumLivePairRead(c *gin.Context, version uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grant, err := h.service.LivePairReadGrant(c.Request.Context(), c.GetString("album.owner"), version.String())
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, grant)
}
