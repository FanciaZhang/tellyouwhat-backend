package albums

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

type assetLifecycleRepository interface {
	CloudAsset(context.Context, string, string, time.Time) (CloudAssetState, error)
	MutateCloudAsset(context.Context, string, string, string, CloudAssetMutation, time.Time) (CloudAssetState, error)
	LibraryChanges(context.Context, string, int64, int) (LibraryChangePage, error)
}

func (h *uploadHTTP) lifecycle(c *gin.Context, mutation bool) assetLifecycleRepository {
	repository, ok := h.service.repository.(assetLifecycleRepository)
	if !ok || h.login == nil || h.login.LibraryLimiter == nil || mutation && !h.login.TrashEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "library_unavailable"})
		return nil
	}
	delay, err := h.login.LibraryLimiter.Check(c.Request.Context(), "library-lifecycle", c.ClientIP())
	if err != nil {
		writeLibraryError(c, ErrReadUnavailable)
		return nil
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "library_rate_limited"})
		return nil
	}
	return repository
}

func (h *uploadHTTP) ListAlbumLibraryChanges(c *gin.Context, params albumhttpapi.ListAlbumLibraryChangesParams) {
	if !emptyAlbumBody(c) {
		return
	}
	after, limit, ok := libraryPageParameters(c, params.After, params.Limit)
	if !ok {
		return
	}
	repository := h.lifecycle(c, false)
	if repository == nil {
		return
	}
	page, err := repository.LibraryChanges(c.Request.Context(), c.GetString("album.owner"), after, limit)
	if err != nil {
		writeLibraryError(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}
func (h *uploadHTTP) GetAlbumCloudAsset(c *gin.Context, assetID albumhttpapi.AssetID) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	repository := h.lifecycle(c, false)
	if repository == nil {
		return
	}
	state, err := repository.CloudAsset(c.Request.Context(), c.GetString("album.owner"), assetID, h.service.now())
	if err != nil {
		writeCloudAssetError(c, err)
		return
	}
	c.JSON(http.StatusOK, state)
}
func (h *uploadHTTP) TrashAlbumCloudAsset(c *gin.Context, assetID albumhttpapi.AssetID) {
	h.mutateCloudAsset(c, assetID, "trash")
}
func (h *uploadHTTP) RestoreAlbumCloudAsset(c *gin.Context, assetID albumhttpapi.AssetID) {
	h.mutateCloudAsset(c, assetID, "restore")
}
func (h *uploadHTTP) mutateCloudAsset(c *gin.Context, asset, action string) {
	repository := h.lifecycle(c, true)
	if repository == nil {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	input, err := decodeCloudAssetMutation(c)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	result, err := repository.MutateCloudAsset(c.Request.Context(), c.GetString("album.owner"), asset, action, input, h.service.now())
	if err != nil {
		writeCloudAssetError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
func writeCloudAssetError(c *gin.Context, err error) {
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"code": "cloud_asset_not_found"})
		return
	}
	writeLibraryError(c, err)
}
func decodeCloudAssetMutation(c *gin.Context) (CloudAssetMutation, error) {
	var input CloudAssetMutation
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return input, ErrInvalidManifest
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return input, ErrInvalidManifest
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return input, ErrInvalidManifest
		}
		seen[name] = true
		switch name {
		case "requestID":
			err = decoder.Decode(&input.RequestID)
		case "expectedRevision":
			err = decoder.Decode(&input.ExpectedRevision)
		default:
			return input, ErrInvalidManifest
		}
		if err != nil {
			return input, ErrInvalidManifest
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return input, ErrInvalidManifest
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || !validOwner(input.RequestID) || input.ExpectedRevision < 1 || input.ExpectedRevision == math.MaxInt64 {
		return input, ErrInvalidManifest
	}
	return input, nil
}
