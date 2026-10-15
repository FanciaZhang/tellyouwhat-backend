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

func (h *uploadHTTP) subscriptionRepository(c *gin.Context) AlbumSubscriptionRepository {
	r, ok := h.service.repository.(AlbumSubscriptionRepository)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "subscription_unavailable"})
		return nil
	}
	return r
}

func (h *uploadHTTP) GetAlbumSubscription(c *gin.Context) {
	if !emptyAlbumBody(c) {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	r := h.subscriptionRepository(c)
	if r == nil {
		return
	}
	result, err := r.CloudAccount(c.Request.Context(), c.GetString("album.owner"), h.service.now().UTC())
	if err != nil {
		writeAlbumError(c, err)
		return
	}
	result.PurchasesEnabled = h.login != nil && h.login.Subscriptions != nil
	c.JSON(http.StatusOK, result)
}

func (h *uploadHTTP) SyncAlbumSubscription(c *gin.Context) {
	if h.login == nil || h.login.Subscriptions == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "subscription_unavailable"})
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeAlbumError(c, ErrInvalidManifest)
		return
	}
	delay, err := h.login.Limiter.Check(c.Request.Context(), "subscription", c.ClientIP())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "subscription_unavailable"})
		return
	}
	if delay > 0 {
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
		c.JSON(http.StatusTooManyRequests, gin.H{"code": "subscription_rate_limited"})
		return
	}
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "json_required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (1<<20)+4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		SignedTransaction string `json:"signedTransaction"`
	}
	if decoder.Decode(&request) != nil || request.SignedTransaction == "" || len(request.SignedTransaction) > 1<<20 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_subscription_request"})
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_subscription_request"})
		return
	}
	result, err := h.login.Subscriptions.Sync(c.Request.Context(), c.GetString("album.owner"), request.SignedTransaction)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "subscription_unavailable"
		switch {
		case errors.Is(err, ErrSubscriptionVerification):
			status, code = http.StatusUnprocessableEntity, "subscription_verification_failed"
		case errors.Is(err, ErrSubscriptionBinding):
			status, code = http.StatusForbidden, "subscription_account_mismatch"
		case errors.Is(err, ErrSubscriptionConflict):
			status, code = http.StatusConflict, "subscription_state_changed"
		}
		c.JSON(status, gin.H{"code": code})
		return
	}
	result.PurchasesEnabled = true
	c.JSON(http.StatusOK, result)
}
