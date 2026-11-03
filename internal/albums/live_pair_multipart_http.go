package albums

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func (h *uploadHTTP) liveMultipartRequest(c *gin.Context, part string) bool {
	if !emptyAlbumBody(c) {
		return false
	}
	if c.Request.URL.RawQuery != "" || (part != "photo" && part != "motion") {
		writeAlbumError(c, ErrInvalidManifest)
		return false
	}
	if h.liveMultipart == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "multipart_unavailable"})
		return false
	}
	return true
}
func (h *uploadHTTP) InitializeAlbumLivePairMultipart(c *gin.Context, id uuid.UUID, part albumhttpapi.ResourceID) {
	if !h.liveMultipartRequest(c, part) {
		return
	}
	result, err := h.liveMultipart.Initialize(c.Request.Context(), c.GetString("album.owner"), id.String(), part)
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) GetAlbumLivePairMultipart(c *gin.Context, id uuid.UUID, part albumhttpapi.ResourceID) {
	if !h.liveMultipartRequest(c, part) {
		return
	}
	result, err := h.liveMultipart.Status(c.Request.Context(), c.GetString("album.owner"), id.String(), part)
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) AuthorizeAlbumLivePairMultipartPart(c *gin.Context, id uuid.UUID, part albumhttpapi.ResourceID, number int) {
	if !h.liveMultipartRequest(c, part) {
		return
	}
	result, err := h.liveMultipart.Part(c.Request.Context(), c.GetString("album.owner"), id.String(), part, number)
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) CompleteAlbumLivePairMultipart(c *gin.Context, id uuid.UUID, part albumhttpapi.ResourceID) {
	if !h.liveMultipartRequest(c, part) {
		return
	}
	result, err := h.liveMultipart.Complete(c.Request.Context(), c.GetString("album.owner"), id.String(), part)
	if err != nil {
		writeLivePairError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
