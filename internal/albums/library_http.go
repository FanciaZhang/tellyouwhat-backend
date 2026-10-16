package albums

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

func (h *uploadHTTP) library(c *gin.Context) LibraryRepository {
	repository, ok := h.service.repository.(LibraryRepository)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "library_unavailable"})
		return nil
	}
	return repository
}
func writeLibraryError(c *gin.Context, err error) {
	if errors.Is(err, ErrReadUnavailable) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "library_unavailable"})
		return
	}
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"code": "asset_version_not_found"})
		return
	}
	writeAlbumError(c, err)
}
func (h *uploadHTTP) ListAlbumAssetVersions(c *gin.Context, params albumhttpapi.ListAlbumAssetVersionsParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	repository := h.library(c)
	if repository == nil {
		return
	}
	result, err := repository.AssetVersions(c.Request.Context(), c.GetString("album.owner"), after, limit)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func (h *uploadHTTP) GetAlbumAssetVersion(c *gin.Context, versionID uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	repository := h.library(c)
	if repository == nil {
		return
	}
	result, err := h.service.verifiedAssetVersion(c.Request.Context(), c.GetString("album.owner"), versionID.String())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *uploadHTTP) CheckAlbumOriginalAvailability(c *gin.Context, versionID uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	result, err := h.service.CheckOriginalAvailability(c.Request.Context(), c.GetString("album.owner"), versionID.String())
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *uploadHTTP) AuthorizeAlbumOriginalRead(c *gin.Context, versionID uuid.UUID, resourceID albumhttpapi.ResourceID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grant, err := h.service.OriginalReadGrant(c.Request.Context(), c.GetString("album.owner"), versionID.String(), resourceID)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, grant)
}

func (h *uploadHTTP) AuthorizeAlbumPhotoPreviewRead(c *gin.Context, versionID uuid.UUID, preset albumhttpapi.PhotoPreset) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grant, err := h.service.PhotoPreviewReadGrant(c.Request.Context(), c.GetString("album.owner"), versionID.String(), PhotoPreset(preset))
	if errors.Is(err, ErrPhotoWriteUncertain) {
		c.JSON(http.StatusConflict, gin.H{"code": "photo_preview_not_ready"})
		return
	}
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, grant)
}

func (h *uploadHTTP) AuthorizeAlbumVideoPreviewRead(c *gin.Context, versionID uuid.UUID, preset albumhttpapi.VideoPreset) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grant, err := h.service.VideoPreviewReadGrant(c.Request.Context(), c.GetString("album.owner"), versionID.String(), VideoPreset(preset))
	if errors.Is(err, ErrVideoNotReady) {
		c.JSON(http.StatusConflict, gin.H{"code": "video_preview_not_ready"})
		return
	}
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, grant)
}

func (h *uploadHTTP) AuthorizeAlbumLivePhotoPreviewRead(c *gin.Context, versionID uuid.UUID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	grant, err := h.service.LivePhotoPreviewReadGrant(c.Request.Context(), c.GetString("album.owner"), versionID.String())
	if errors.Is(err, ErrLivePhotoNotReady) {
		c.JSON(http.StatusConflict, gin.H{"code": "live_photo_preview_not_ready"})
		return
	}
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, grant)
}

func libraryPageParameters(c *gin.Context, afterValue *int64, limitValue *int) (int64, int, bool) {
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeAlbumError(c, ErrInvalidManifest)
		return 0, 0, false
	}
	for name, values := range values {
		if (name != "after" && name != "limit") || len(values) != 1 || values[0] == "" {
			writeAlbumError(c, ErrInvalidManifest)
			return 0, 0, false
		}
	}
	after, limit := int64(0), 50
	if afterValue != nil {
		after = *afterValue
	}
	if limitValue != nil {
		limit = *limitValue
	}
	if after < 0 || limit < 1 || limit > 100 {
		writeAlbumError(c, ErrInvalidManifest)
		return 0, 0, false
	}
	return after, limit, true
}
