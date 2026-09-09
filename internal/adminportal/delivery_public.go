package adminportal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

// Links and receipts use distinct MAC domains. Tokens are delivered in URL fragments
// and JSON bodies, never in server-visible paths, query strings or database rows.
func (s *Server) deliveryToken(kind, app, id string) string {
	payload := app + "." + id
	mac := hmac.New(sha256.New, s.config.PreviewSigningKey)
	mac.Write([]byte("offer-delivery/" + kind + "/" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) parseDeliveryToken(kind, token string) (app, id string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(s.config.PreviewSigningKey) < 32 {
		return "", "", false
	}
	app, id = parts[0], parts[1]
	if s.offers[app] == nil {
		return "", "", false
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", "", false
	}
	return app, id, hmac.Equal([]byte(s.deliveryToken(kind, app, id)), []byte(token))
}

type claimCommand struct {
	Action     string `json:"action"`
	Token      string `json:"token"`
	RequestKey string `json:"requestKey"`
	Name       string `json:"name"`
	Contact    string `json:"contact"`
	Note       string `json:"note"`
	Consent    bool   `json:"consent"`
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
	store := s.config.Delivery
	ctx := c.Request.Context()
	now := s.now()
	if in.Action == "inspect" || in.Action == "apply" {
		app, id, ok := s.parseDeliveryToken("link", in.Token)
		if !ok {
			writeFailure(c.Writer, 404, "claim_not_found", "领取链接无效或已失效")
			return
		}
		link, err := store.GetLink(ctx, app, id)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		pool, err := store.GetPool(ctx, app, link.PoolID)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		if in.Action == "inspect" {
			open := link.RevokedAt == nil && link.ExpiresAt.After(now) && pool.Active && pool.ExpiresAt.After(now) && link.Applications < link.MaxApplications
			writeJSON(c.Writer, 200, map[string]any{"title": pool.OfferName, "app": app, "accepting": open, "expiresAt": link.ExpiresAt, "channel": link.Label})
			return
		}
		if !in.Consent {
			writeFailure(c.Writer, 422, "claim_consent_required", "请先确认领取信息的用途")
			return
		}
		recipient := offerdelivery.Recipient{Name: in.Name, Contact: in.Contact, Note: in.Note}
		r, err := store.ApplyLink(ctx, app, id, in.RequestKey, recipient, now)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		writeJSON(c.Writer, 200, map[string]any{"receiptToken": s.deliveryToken("receipt", app, r.ID), "status": r.Status})
		return
	}
	if in.Action != "status" && in.Action != "claim" && in.Action != "feedback" {
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	app, id, ok := s.parseDeliveryToken("receipt", in.Token)
	if !ok {
		writeFailure(c.Writer, 404, "claim_not_found", "查询回执无效或已失效")
		return
	}
	r, err := store.Get(ctx, app, id)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	if r.Source != "link" || now.After(r.RequestedAt.Add(90*24*time.Hour)) {
		writeFailure(c.Writer, 404, "claim_not_found", "查询回执无效或已失效")
		return
	}
	pool, err := store.GetPool(ctx, app, r.PoolID)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	result := map[string]any{"title": pool.OfferName, "name": r.Recipient.Name, "status": r.Status, "deliveredAt": r.DeliveredAt, "reportedRedeemedAt": r.ReportedRedeemedAt, "receiptExpiresAt": r.RequestedAt.Add(90 * 24 * time.Hour)}
	switch in.Action {
	case "claim":
		if !pool.Active || !pool.ExpiresAt.After(now) {
			writeFailure(c.Writer, 409, "claim_expired", "这个 Offer 已停用或过期，请联系发放人")
			return
		}
		code, err := store.Reveal(ctx, app, id, "applicant", now)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		if r.Status == "assigned" {
			r, err = store.Transition(ctx, app, id, "applicant", "deliver", r.Version, now)
			if err != nil {
				deliveryFailure(c.Writer, err)
				return
			}
		}
		result["status"] = r.Status
		result["deliveredAt"] = r.DeliveredAt
		result["code"] = code
	case "feedback":
		if r.ReportedRedeemedAt == nil {
			r, err = store.Transition(ctx, app, id, "applicant", "report_redeemed", r.Version, now)
			if err != nil {
				deliveryFailure(c.Writer, err)
				return
			}
		}
		result["reportedRedeemedAt"] = r.ReportedRedeemedAt
	}
	writeJSON(c.Writer, 200, result)
}
