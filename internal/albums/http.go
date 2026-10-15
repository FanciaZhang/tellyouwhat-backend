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
	"github.com/tellyouwhat/backend/internal/albumhttpapi"
)

type AccountIdentity struct{ AppID, AccountID string }
type AccountAuthenticator interface {
	Authenticate(context.Context, *http.Request) (AccountIdentity, error)
}

// NewHTTPRouter requires a verified account authenticator. There is no header,
// device-ID, or debug-owner fallback. Deployment wiring remains opt-in.
func NewHTTPRouter(host string, auth AccountAuthenticator, service *UploadService) (*gin.Engine, error) {
	return newHTTPRouter(host, auth, service, nil, nil)
}

func newHTTPRouter(host string, auth AccountAuthenticator, service *UploadService, login *LoginHTTPOptions, multipart *MultipartService) (*gin.Engine, error) {
	if canonicalHost(host) == "" || auth == nil || service == nil {
		return nil, errors.New("album account authentication, host and service are required")
	}
	router := gin.New()
	router.RedirectTrailingSlash = false
	var proxies []string
	if login != nil {
		proxies = login.TrustedProxies
	}
	if err := router.SetTrustedProxies(proxies); err != nil {
		return nil, err
	}
	router.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if canonicalHost(c.Request.Host) != canonicalHost(host) {
			c.AbortWithStatusJSON(http.StatusMisdirectedRequest, gin.H{"code": "unknown_app_host"})
			return
		}
		if sharePublicRequest(c, login) {
			if !c.IsAborted() {
				c.Next()
			}
			return
		}
		// Exact deletion retries use the originally accepted credential after
		// revocation. The handler authenticates it inside the deletion transaction.
		if c.Request.Method == http.MethodPost && (c.Request.URL.Path == "/v1/albums/account/deletion" || c.Request.URL.Path == "/v1/albums/account/deletion/withdrawal") {
			c.Next()
			return
		}
		// Logout validates and revokes only the presented credential, even after expiry.
		if login != nil && c.Request.Method == http.MethodPost && (c.Request.URL.Path == "/v1/albums/auth/apple/notifications" || c.Request.URL.Path == "/v1/albums/auth/challenges" || c.Request.URL.Path == "/v1/albums/auth/apple" || c.Request.URL.Path == "/v1/albums/auth/logout" || c.Request.URL.Path == "/v1/albums/subscription/notifications") {
			c.Next()
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
	var liveMultipart *LivePairMultipartService
	if multipart != nil {
		if repo, ok := service.repository.(*MySQLUploads); ok {
			if provider, ok := service.objects.(multipartRecoveryProvider); ok {
				var err error
				liveMultipart, err = newLivePairMultipartService(repo, provider, service.now)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	albumhttpapi.RegisterHandlersWithOptions(router, &uploadHTTP{service: service, login: login, multipart: multipart, liveMultipart: liveMultipart}, albumhttpapi.GinServerOptions{
		ErrorHandler: func(c *gin.Context, _ error, status int) {
			c.JSON(status, gin.H{"code": "invalid_upload_request"})
		},
	})
	return router, nil
}

type uploadHTTP struct {
	liveMultipart *LivePairMultipartService
	multipart     *MultipartService
	service       *UploadService
	login         *LoginHTTPOptions
}

func (h *uploadHTTP) CreateAlbumUpload(c *gin.Context) {
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
	value, err := h.service.Create(c.Request.Context(), c.GetString("album.owner"), request.RequestID, manifest)
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusCreated, value)
}
func (h *uploadHTTP) GetAlbumUpload(c *gin.Context, id albumhttpapi.UploadID) {
	value, err := h.service.Get(c.Request.Context(), c.GetString("album.owner"), id.String())
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
func (h *uploadHTTP) AuthorizeAlbumUploadResources(c *gin.Context, id albumhttpapi.UploadID) {
	if !emptyAlbumBody(c) {
		return
	}
	value, err := h.service.Grants(c.Request.Context(), c.GetString("album.owner"), id.String())
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"grants": value})
}
func (h *uploadHTTP) SubmitAlbumUpload(c *gin.Context, id albumhttpapi.UploadID) {
	if !emptyAlbumBody(c) {
		return
	}
	value, err := h.service.Submit(c.Request.Context(), c.GetString("album.owner"), id.String())
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, value)
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
	case errors.Is(err, ErrSubscriptionRequired):
		status, code = http.StatusForbidden, "cloud_subscription_required"
	case errors.Is(err, ErrSubscriptionStale):
		status, code = http.StatusServiceUnavailable, "cloud_subscription_refresh_pending"
		c.Header("Retry-After", "30")
	case errors.Is(err, ErrMultipartPending):
		status, code = http.StatusServiceUnavailable, "multipart_pending"
		c.Header("Retry-After", "30")
	case errors.Is(err, ErrIncompleteBackup):
		status, code = http.StatusConflict, "multipart_incomplete"
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
