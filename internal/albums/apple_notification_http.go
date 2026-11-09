package albums

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *uploadHTTP) ReceiveAlbumAppleNotification(c *gin.Context) {
	if h.login == nil || h.login.AppleNotifications == nil {
		c.JSON(503, gin.H{"code": "apple_unavailable"})
		return
	}
	if c.Request.URL.RawQuery != "" {
		c.JSON(400, gin.H{"code": "invalid_apple_notification"})
		return
	}
	limiter := h.login.NotificationLimiter
	if limiter == nil {
		limiter = h.login.Limiter
	}
	delay, err := limiter.Check(c.Request.Context(), "apple-notification", c.ClientIP())
	if err != nil {
		c.JSON(503, gin.H{"code": "apple_unavailable"})
		return
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(429, gin.H{"code": "apple_notification_rate_limited"})
		return
	}
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		c.JSON(415, gin.H{"code": "json_required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (32<<10)+4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Payload string `json:"payload"`
	}
	if decoder.Decode(&request) != nil || request.Payload == "" || len(request.Payload) > 32<<10 {
		c.JSON(400, gin.H{"code": "invalid_apple_notification"})
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		c.JSON(400, gin.H{"code": "invalid_apple_notification"})
		return
	}
	if err := h.login.AppleNotifications.receive(c.Request.Context(), request.Payload); err != nil {
		if errors.Is(err, errAppleNotification) {
			c.JSON(422, gin.H{"code": "apple_notification_rejected"})
			return
		}
		// Apple retries failures; acknowledgement follows the committed update.
		c.JSON(503, gin.H{"code": "apple_unavailable"})
		return
	}
	c.Status(http.StatusOK)
}
