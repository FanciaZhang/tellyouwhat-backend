package albums

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type AccountIdentity struct{ AppID, AccountID string }
type AccountAuthenticator interface {
	Authenticate(context.Context, *http.Request) (AccountIdentity, error)
}

// NewHTTPRouter requires a verified account authenticator. There is no header,
// device-ID, or debug-owner fallback. Deployment wiring remains opt-in.
func NewHTTPRouter(host string, auth AccountAuthenticator, service *UploadService) (*gin.Engine, error) {
	if canonicalHost(host) == "" || auth == nil || service == nil {
		return nil, errors.New("album account authentication, host and service are required")
	}
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if canonicalHost(c.Request.Host) != canonicalHost(host) {
			c.AbortWithStatusJSON(http.StatusMisdirectedRequest, gin.H{"code": "unknown_app_host"})
			return
		}
		principal, err := auth.Authenticate(c.Request.Context(), c.Request)
		if err != nil || !validOwner(principal.AccountID) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
			return
		}
		if principal.AppID != "albums" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": "app_scope_mismatch"})
			return
		}
		c.Set("album.owner", principal.AccountID)
		c.Next()
	})
	router.POST("/v1/albums/uploads", func(c *gin.Context) {
		var request struct {
			RequestID string `json:"requestID"`
			Manifest  struct {
				Manifest
				Edited *bool `json:"edited"`
			} `json:"manifest"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		decoder := json.NewDecoder(c.Request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeAlbumError(c, ErrInvalidManifest)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeAlbumError(c, ErrInvalidManifest)
			return
		}
		// Missing or null edited must not silently become false: that could let
		// a client omit the current rendered resources of an edited asset.
		if request.Manifest.Edited == nil {
			writeAlbumError(c, ErrInvalidManifest)
			return
		}
		manifest := request.Manifest.Manifest
		manifest.Edited = *request.Manifest.Edited
		value, err := service.Create(c.Request.Context(), c.GetString("album.owner"), request.RequestID, manifest)
		if err != nil {
			writeAlbumError(c, err)
			return
		}
		c.JSON(http.StatusCreated, value)
	})
	router.GET("/v1/albums/uploads/:id", func(c *gin.Context) {
		value, err := service.Get(c.Request.Context(), c.GetString("album.owner"), c.Param("id"))
		if err != nil {
			writeAlbumError(c, err)
			return
		}
		c.JSON(http.StatusOK, value)
	})
	router.POST("/v1/albums/uploads/:id/grants", func(c *gin.Context) {
		if !emptyAlbumBody(c) {
			return
		}
		value, err := service.Grants(c.Request.Context(), c.GetString("album.owner"), c.Param("id"))
		if err != nil {
			writeAlbumError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"grants": value})
	})
	router.POST("/v1/albums/uploads/:id/submit", func(c *gin.Context) {
		if !emptyAlbumBody(c) {
			return
		}
		value, err := service.Submit(c.Request.Context(), c.GetString("album.owner"), c.Param("id"))
		if err != nil {
			writeAlbumError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, value)
	})
	return router, nil
}

func emptyAlbumBody(c *gin.Context) bool {
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
	if err != nil || len(data) != 0 {
		writeAlbumError(c, ErrInvalidManifest)
		return false
	}
	return true
}
func canonicalHost(value string) string {
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
func writeAlbumError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "album_service_unavailable"
	switch {
	case errors.Is(err, ErrOwner):
		status, code = http.StatusForbidden, "account_unavailable"
	case errors.Is(err, ErrInvalidManifest):
		status, code = http.StatusUnprocessableEntity, "invalid_manifest"
	case errors.Is(err, ErrQuota):
		status, code = http.StatusForbidden, "storage_quota_exceeded"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "upload_not_found"
	case errors.Is(err, ErrExpired):
		status, code = http.StatusGone, "upload_expired"
	case errors.Is(err, ErrConflict), errors.Is(err, ErrLease):
		status, code = http.StatusConflict, "upload_state_conflict"
	}
	c.JSON(status, gin.H{"code": code})
}
