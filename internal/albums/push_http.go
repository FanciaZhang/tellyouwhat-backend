package albums

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *uploadHTTP) allowPushRegistration(c *gin.Context) bool {
	if h.login == nil || h.login.PushRegistrations == nil || h.login.PushLimiter == nil {
		writePushError(c, errPushRegistration)
		return false
	}
	delay, err := h.login.PushLimiter.Check(c.Request.Context(), "push-registration", c.ClientIP())
	if err != nil {
		writePushError(c, errPushRegistration)
		return false
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "push_registration_rate_limited"})
		return false
	}
	if c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "push_registration_invalid"})
		return false
	}
	return true
}

func writePushError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "push_registration_unavailable"
	if errors.Is(err, ErrSession) {
		status, code = http.StatusUnauthorized, "account_authentication_required"
	} else if errors.Is(err, errPushRegistrationConflict) {
		status, code = http.StatusConflict, "push_registration_changed"
	}
	c.JSON(status, gin.H{"code": code})
}

func (h *uploadHTTP) GetAlbumPushRegistration(c *gin.Context) {
	if !h.allowPushRegistration(c) {
		return
	}
	if _, err := c.Request.Body.Read(make([]byte, 1)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"code": "push_registration_invalid"})
		return
	}
	credential, err := requestSessionToken(c.Request)
	if err != nil {
		writePushError(c, err)
		return
	}
	current, err := h.login.PushRegistrations.current(c.Request.Context(), credential, time.Now().UTC())
	if err != nil {
		writePushError(c, err)
		return
	}
	c.JSON(http.StatusOK, current)
}

func (h *uploadHTTP) UpdateAlbumPushRegistration(c *gin.Context) {
	if !h.allowPushRegistration(c) {
		return
	}
	u, err := decodePushUpdate(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "push_registration_invalid"})
		return
	}
	credential, err := requestSessionToken(c.Request)
	if err != nil {
		writePushError(c, err)
		return
	}
	current, err := h.login.PushRegistrations.update(c.Request.Context(), credential, u, time.Now().UTC())
	if err != nil {
		writePushError(c, err)
		return
	}
	c.JSON(http.StatusOK, current)
}

func decodePushUpdate(c *gin.Context) (albumPushUpdate, error) {
	invalid := errPushRegistration
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		return albumPushUpdate{}, invalid
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	d := json.NewDecoder(c.Request.Body)
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return albumPushUpdate{}, invalid
	}
	var request, device *string
	var revision *int64
	var enabled *bool
	seen := make(map[string]bool)
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return albumPushUpdate{}, invalid
		}
		seen[name] = true
		var value any
		switch name {
		case "requestID":
			value = &request
		case "deviceToken":
			value = &device
		case "expectedRevision":
			value = &revision
		case "enabled":
			value = &enabled
		default:
			return albumPushUpdate{}, invalid
		}
		if err := d.Decode(value); err != nil {
			return albumPushUpdate{}, invalid
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return albumPushUpdate{}, invalid
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF || request == nil || revision == nil || enabled == nil ||
		!validOwner(*request) || *revision < 0 || *revision >= math.MaxInt64 || seen["deviceToken"] && device == nil {
		return albumPushUpdate{}, invalid
	}
	u := albumPushUpdate{RequestID: *request, ExpectedRevision: *revision, Enabled: *enabled}
	if *enabled {
		if device == nil {
			return u, invalid
		}
		if _, err := pushTokenDigest(*device); err != nil {
			return u, invalid
		}
		u.DeviceToken = *device
	} else if device != nil {
		return u, invalid
	}
	return u, nil
}
