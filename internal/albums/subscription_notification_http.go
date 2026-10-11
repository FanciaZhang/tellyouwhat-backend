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

func (h *uploadHTTP) ReceiveAlbumSubscriptionNotification(c *gin.Context) {
	if h.login == nil || h.login.Subscriptions == nil {
		c.JSON(503, gin.H{"code": "subscription_unavailable"})
		return
	}
	if c.Request.URL.RawQuery != "" {
		c.JSON(400, gin.H{"code": "invalid_subscription_notification"})
		return
	}
	limiter := h.login.NotificationLimiter
	if limiter == nil {
		limiter = h.login.Limiter
	}
	delay, err := limiter.Check(c.Request.Context(), "notification", c.ClientIP())
	if err != nil {
		c.JSON(503, gin.H{"code": "subscription_unavailable"})
		return
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(429, gin.H{"code": "subscription_notification_rate_limited"})
		return
	}
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		c.JSON(415, gin.H{"code": "json_required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (1<<20)+4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		SignedPayload string `json:"signedPayload"`
	}
	if decoder.Decode(&request) != nil || request.SignedPayload == "" || len(request.SignedPayload) > 1<<20 {
		c.JSON(400, gin.H{"code": "invalid_subscription_notification"})
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		c.JSON(400, gin.H{"code": "invalid_subscription_notification"})
		return
	}
	if err := h.login.Subscriptions.receiveNotification(c.Request.Context(), request.SignedPayload); err != nil {
		if errors.Is(err, ErrSubscriptionVerification) || errors.Is(err, ErrSubscriptionBinding) {
			c.JSON(422, gin.H{"code": "subscription_notification_rejected"})
			return
		}
		// Apple retries failures; acknowledgement follows the committed update.
		c.JSON(503, gin.H{"code": "subscription_unavailable"})
		return
	}
	c.Status(http.StatusOK)
}
