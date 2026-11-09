package albums

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func NewHTTPRouterWithMultipart(host string, auth AccountAuthenticator, uploads *UploadService, multipart *MultipartService) (*gin.Engine, error) {
	if multipart == nil {
		return nil, errors.New("multipart service required")
	}
	return newHTTPRouter(host, auth, uploads, nil, multipart)
}

func (h *uploadHTTP) multipartRequest(c *gin.Context, resource string) bool {
	if !emptyAlbumBody(c) {
		return false
	}
	if c.Request.URL.RawQuery != "" || !resourceIDPattern.MatchString(resource) {
		writeAlbumError(c, ErrInvalidManifest)
		return false
	}
	if h.multipart == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "multipart_unavailable"})
		return false
	}
	return true
}
func (h *uploadHTTP) InitializeAlbumMultipart(c *gin.Context, id albumhttpapi.UploadID, resource albumhttpapi.ResourceID) {
	if !h.multipartRequest(c, resource) {
		return
	}
	result, err := h.multipart.Initialize(c.Request.Context(), c.GetString("album.owner"), id.String(), resource)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) GetAlbumMultipart(c *gin.Context, id albumhttpapi.UploadID, resource albumhttpapi.ResourceID) {
	if !h.multipartRequest(c, resource) {
		return
	}
	result, err := h.multipart.Status(c.Request.Context(), c.GetString("album.owner"), id.String(), resource)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) AuthorizeAlbumMultipartPart(c *gin.Context, id albumhttpapi.UploadID, resource albumhttpapi.ResourceID, number int) {
	if !h.multipartRequest(c, resource) {
		return
	}
	result, err := h.multipart.Part(c.Request.Context(), c.GetString("album.owner"), id.String(), resource, number)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) CompleteAlbumMultipart(c *gin.Context, id albumhttpapi.UploadID, resource albumhttpapi.ResourceID) {
	if !h.multipartRequest(c, resource) {
		return
	}
	result, err := h.multipart.Complete(c.Request.Context(), c.GetString("album.owner"), id.String(), resource)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
