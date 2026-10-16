package albums

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func publicSharePath(path string) bool {
	return path == "/v1/albums/share-links/lookup" || path == "/v1/albums/share-links/claims" || path == "/v1/albums/share-links/read-grants"
}

// CORS is scoped to the three recipient POSTs. No cookies, account APIs or
// bearer Authorization header are admitted by browser preflight.
func sharePublicRequest(c *gin.Context, options *LoginHTTPOptions) bool {
	if options == nil || !publicSharePath(c.Request.URL.Path) {
		return false
	}
	if options.Shares != nil {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if origin != options.Shares.siteOrigin {
				c.AbortWithStatusJSON(403, gin.H{"code": "share_origin_denied"})
				return true
			}
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Expose-Headers", "Retry-After")
			c.Header("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			if origin == "" || c.GetHeader("Access-Control-Request-Method") != "POST" {
				c.AbortWithStatusJSON(403, gin.H{"code": "share_origin_denied"})
				return true
			}
			for _, header := range strings.Split(c.GetHeader("Access-Control-Request-Headers"), ",") {
				value := strings.TrimSpace(strings.ToLower(header))
				if value != "" && value != "content-type" {
					c.AbortWithStatusJSON(403, gin.H{"code": "share_origin_denied"})
					return true
				}
			}
			c.Header("Access-Control-Allow-Methods", "POST")
			c.Header("Access-Control-Allow-Headers", "Content-Type")
			c.Header("Access-Control-Max-Age", "600")
			c.AbortWithStatus(204)
			return true
		}
	}
	return c.Request.Method == http.MethodPost
}
func (h *uploadHTTP) allowShare(c *gin.Context, operation string) bool {
	if h.login == nil || h.login.Shares == nil {
		writeShareError(c, ErrShareUnavailable)
		return false
	}
	limiter := h.login.ShareLimiter
	if operation == "share-claim" {
		limiter = h.login.ShareClaimLimiter
	}
	if limiter == nil {
		writeShareError(c, ErrShareUnavailable)
		return false
	}
	delay, err := limiter.Check(c.Request.Context(), operation, c.ClientIP())
	if err != nil {
		writeShareError(c, ErrShareUnavailable)
		return false
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(429, gin.H{"code": "share_rate_limited"})
		return false
	}
	return true
}
func decodeShareJSON(c *gin.Context, value any) bool {
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" || c.Request.URL.RawQuery != "" {
		writeShareError(c, ErrShareInput)
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeShareError(c, ErrShareInput)
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeShareError(c, ErrShareInput)
		return false
	}
	return true
}
func writeShareError(c *gin.Context, err error) {
	status, code := 503, "share_unavailable"
	switch {
	case errors.Is(err, ErrShareInput):
		status, code = 422, "share_request_invalid"
	case errors.Is(err, ErrSharePassword):
		status, code = 422, "share_password_rejected"
	case errors.Is(err, ErrShareNotAvailable), errors.Is(err, ErrNotFound), errors.Is(err, ErrOwner):
		status, code = 404, "share_not_available"
	case errors.Is(err, ErrShareLimit):
		status, code = 410, "share_claim_limit_reached"
	case errors.Is(err, ErrShareClaimExpired):
		status, code = 410, "share_claim_expired"
	case errors.Is(err, ErrShareLinksLimit):
		status, code = 409, "share_link_limit_reached"
	case errors.Is(err, ErrShareConflict):
		status, code = 409, "share_request_conflict"
	}
	if status == 503 {
		c.Header("Retry-After", "30")
	}
	c.JSON(status, gin.H{"code": code})
}
func (h *uploadHTTP) CreateAlbumShareLink(c *gin.Context, versionID uuid.UUID) {
	if !h.allowShare(c, "share-owner") {
		return
	}
	var input ShareCreateInput
	if !decodeShareJSON(c, &input) {
		return
	}
	value, err := h.login.Shares.Create(c.Request.Context(), c.GetString("album.owner"), versionID.String(), input)
	if err != nil {
		writeShareError(c, err)
		return
	}
	c.JSON(201, value)
}
func (h *uploadHTTP) ListAlbumShareLinks(c *gin.Context, versionID uuid.UUID) {
	if !h.allowShare(c, "share-owner") {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeShareError(c, ErrShareInput)
		return
	}
	value, err := h.login.Shares.List(c.Request.Context(), c.GetString("album.owner"), versionID.String())
	if err != nil {
		writeShareError(c, err)
		return
	}
	c.JSON(200, gin.H{"links": value})
}
func (h *uploadHTTP) RevokeAlbumShareLink(c *gin.Context, shareID uuid.UUID) {
	if !h.allowShare(c, "share-owner") || !emptyAlbumBody(c) {
		return
	}
	if err := h.login.Shares.Revoke(c.Request.Context(), c.GetString("album.owner"), shareID.String()); err != nil {
		writeShareError(c, err)
		return
	}
	c.Status(204)
}
func (h *uploadHTTP) LookupAlbumShareLink(c *gin.Context) {
	if !h.allowShare(c, "share-read") {
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if !decodeShareJSON(c, &input) {
		return
	}
	value, err := h.login.Shares.Lookup(c.Request.Context(), input.Token)
	if err != nil {
		writeShareError(c, err)
		return
	}
	c.JSON(200, value)
}
func (h *uploadHTTP) ClaimAlbumShareLink(c *gin.Context) {
	if !h.allowShare(c, "share-claim") {
		return
	}
	var input struct {
		Token     string `json:"token"`
		Password  string `json:"password,omitempty"`
		RequestID string `json:"requestID"`
	}
	if !decodeShareJSON(c, &input) {
		return
	}
	value, err := h.login.Shares.Claim(c.Request.Context(), input.Token, input.Password, input.RequestID)
	if err != nil {
		writeShareError(c, err)
		return
	}
	c.JSON(200, value)
}
func (h *uploadHTTP) AuthorizeAlbumSharedReads(c *gin.Context) {
	if !h.allowShare(c, "share-read") {
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if !decodeShareJSON(c, &input) {
		return
	}
	values, err := h.login.Shares.ReadGrants(c.Request.Context(), input.Token)
	if err != nil {
		writeShareError(c, err)
		return
	}
	c.JSON(200, gin.H{"resources": values})
}
