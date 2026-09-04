package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

func (s *Server) registerVoiceRoutes(router gin.IRouter) {
	router.POST("/v1/journal/voice/sessions", func(c *gin.Context) {
		if s.voice == nil || s.voiceEntitlements == nil {
			writeTransportError(c, 503, "voice_not_enabled", "voice service is not enabled")
			return
		}
		requestID, err := uuid.Parse(c.GetHeader("X-Tellyouwhat-Request-ID"))
		if err != nil {
			writeTransportError(c, 422, "invalid_parameter", "invalid request id")
			return
		}
		principal, failure := s.apiAuthenticate(c, requestID)
		if failure != nil {
			c.JSON(failure.status, failure.platformResponse())
			return
		}
		if failure = s.apiRequireManagedEntitlement(c, principal, requestID.String()); failure != nil {
			c.JSON(failure.status, failure.platformResponse())
			return
		}
		if failure = s.apiRequireConsents(c, principal, s.requiredConsentScopes, requestID.String()); failure != nil {
			c.JSON(failure.status, failure.platformResponse())
			return
		}
		var input struct {
			SessionID      string `json:"sessionID"`
			ConsentVersion string `json:"consentVersion"`
		}
		if json.Unmarshal(rawRequestBody(c), &input) != nil || input.ConsentVersion != voice.Version {
			writeTransportError(c, 422, "voice_consent_required", "voice consent is required")
			return
		}
		record, ok, err := s.voiceEntitlements.Get(c, principal.KeyID)
		if err != nil || !ok || !record.ExpiresAt.After(time.Now()) {
			writeTransportError(c, 403, "managed_subscription_required", "subscription required")
			return
		}
		if record.StartedAt.IsZero() {
			writeTransportError(c, 409, "voice_subscription_sync_required", "restore the subscription to synchronize the purchase date")
			return
		}
		owner := record.TransactionID
		if owner == "" && record.Environment == "development" {
			owner = principal.KeyID
		}
		if owner == "" {
			writeTransportError(c, 403, "managed_subscription_required", "subscription required")
			return
		}
		ticket, err := s.voice.Issue(c, voice.Identity{Owner: record.Environment + ":" + owner, KeyID: principal.KeyID, Anchor: record.StartedAt, ExpiresAt: record.ExpiresAt}, input.SessionID)
		if err != nil {
			writeTransportError(c, 503, "voice_unavailable", "voice session could not be created")
			return
		}
		c.JSON(http.StatusCreated, ticket)
	})
	router.GET("/v1/journal/voice/sessions/:sessionID/stream", func(c *gin.Context) {
		if s.voice == nil {
			writeTransportError(c, 503, "voice_not_enabled", "voice service is not enabled")
			return
		}
		s.voice.Serve(c.Writer, c.Request, c.Param("sessionID"))
	})
}
