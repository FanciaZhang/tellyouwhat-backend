package albums

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type SessionRevoker interface {
	Revoke(context.Context, string) error
}
type LoginHTTPOptions struct {
	Multipart           *MultipartService
	Service             *LoginService
	Limiter             LoginRateLimiter
	Sessions            SessionRevoker
	Subscriptions       *AlbumSubscriptionService
	NotificationLimiter LoginRateLimiter
	Shares              *ShareService
	ShareLimiter        LoginRateLimiter
	ShareClaimLimiter   LoginRateLimiter
	PushRegistrations   *albumPushRegistrations
	PushLimiter         LoginRateLimiter
	// Empty means direct peer only. Set only actual deployment proxy networks.
	TrustedProxies []string
}

func NewHTTPRouterWithLogin(host string, auth AccountAuthenticator, uploads *UploadService, options LoginHTTPOptions) (*gin.Engine, error) {
	if options.Service == nil || options.Limiter == nil || options.Sessions == nil {
		return nil, errors.New("album login, limiter and revoker are required")
	}
	if options.Shares != nil && (options.ShareLimiter == nil || options.ShareClaimLimiter == nil) {
		return nil, ErrShareUnavailable
	}
	if options.PushRegistrations != nil && options.PushLimiter == nil {
		return nil, errPushRegistration
	}
	options.TrustedProxies = append([]string(nil), options.TrustedProxies...)
	return newHTTPRouter(host, auth, uploads, &options, options.Multipart)
}
func (h *uploadHTTP) allowLogin(c *gin.Context, operation string) bool {
	if h.login == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "login_unavailable"})
		return false
	}
	delay, err := h.login.Limiter.Check(c.Request.Context(), operation, c.ClientIP())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "login_unavailable"})
		return false
	}
	if delay > 0 {
		seconds := (delay + time.Second - 1) / time.Second
		c.Header("Retry-After", strconv.FormatInt(int64(seconds), 10))
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "login_rate_limited"})
		return false
	}
	return true
}
func (h *uploadHTTP) CreateAlbumLoginChallenge(c *gin.Context) {
	if !h.allowLogin(c, "challenge") || !emptyAlbumBody(c) {
		return
	}
	challenge, err := h.login.Service.Challenge(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "login_unavailable"})
		return
	}
	c.JSON(http.StatusCreated, challenge)
}
func (h *uploadHTTP) LoginAlbumWithApple(c *gin.Context) {
	if !h.allowLogin(c, "login") {
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "json_required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 80<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request AppleLoginRequest
	if err := decoder.Decode(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_login_request"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_login_request"})
		return
	}
	session, err := h.login.Service.Login(c.Request.Context(), request)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "apple_login_failed"})
		return
	}
	c.JSON(http.StatusOK, session)
}
func (h *uploadHTTP) GetAlbumAccount(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"accountID": c.GetString("album.owner")})
}
func (h *uploadHTTP) LogoutAlbumSession(c *gin.Context) {
	if h.login == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "login_unavailable"})
		return
	}
	if !emptyAlbumBody(c) {
		return
	}
	token, err := requestSessionToken(c.Request)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
		return
	}
	if err := h.login.Sessions.Revoke(c.Request.Context(), token); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "logout_unavailable"})
		return
	}
	c.Status(http.StatusNoContent)
}
