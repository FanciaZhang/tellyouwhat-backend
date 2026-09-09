package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

// Claim tokens bind an App, delivery record and revocation generation. Tokens travel
// in URL fragments and JSON bodies, never server-visible URLs or database rows.
func (s *Server) deliveryToken(app, id string, generation int) string {
	payload := app + "." + id + "." + strconv.Itoa(generation)
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write([]byte("offer-delivery/direct-claim/" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) parseDeliveryToken(token string) (app, id string, generation int, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || len(s.config.PreviewSigningKey) < 32 {
		return "", "", 0, false
	}
	app, id = parts[0], parts[1]
	if s.offers[app] == nil {
		return "", "", 0, false
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", "", 0, false
	}
	var err error
	generation, err = strconv.Atoi(parts[2])
	if err != nil || generation < 1 {
		return "", "", 0, false
	}
	return app, id, generation, hmac.Equal([]byte(s.deliveryToken(app, id, generation)), []byte(token))
}

type claimCommand struct {
	Action string `json:"action"`
	Token  string `json:"token"`
}

func (s *Server) PublicOfferClaim(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if s.config.Delivery == nil {
		writeFailure(c.Writer, 503, "claim_unavailable", "领取服务暂时不可用")
		return
	}
	var in claimCommand
	if !decodeJSON(c.Writer, c.Request, &in) {
		return
	}
	if in.Action != "status" && in.Action != "claim" && in.Action != "feedback" {
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	app, id, generation, ok := s.parseDeliveryToken(in.Token)
	if !ok {
		writeFailure(c.Writer, 404, "claim_not_found", "领取链接无效或已失效")
		return
	}
	store := s.config.Delivery
	ctx := c.Request.Context()
	now := s.now()
	r, code, err := store.AccessClaim(ctx, app, id, generation, in.Action, now)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	pool, err := store.GetPool(ctx, app, r.PoolID)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	result := map[string]any{"title": pool.OfferName, "status": r.Status, "deliveredAt": r.DeliveredAt, "claimedAt": r.ClaimedAt, "reportedRedeemedAt": r.ReportedRedeemedAt, "receiptExpiresAt": r.ClaimExpiresAt}
	if in.Action == "claim" {
		result["code"] = code
		if appAppleID, _ := offerScope(s.offers[app]); appAppleID != "" && pool.Environment == "production" {
			result["redeemURL"] = "https://apps.apple.com/redeem?" + url.Values{"ctx": {"offercodes"}, "id": {appAppleID}, "code": {code}}.Encode()
		}
	}
	writeJSON(c.Writer, 200, result)
}
