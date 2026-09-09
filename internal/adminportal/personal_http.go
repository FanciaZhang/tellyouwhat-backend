package adminportal

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

func (s *Server) ListPersonalDeliveries(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, _ adminhttpapi.ListPersonalDeliveriesParams) {
	if _, ok := s.deliveryAccess(c, app, false, false); !ok {
		return
	}
	store := s.config.Delivery
	ctx := c.Request.Context()
	rows, next, err := store.ListPersonal(ctx, app, offer, c.Query("cursor"))
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	out := []any{}
	for _, row := range rows {
		item := map[string]any{"delivery": row}
		if row.RequestID != "" {
			request, err := store.Get(ctx, app, row.RequestID)
			if err != nil {
				deliveryFailure(c.Writer, err)
				return
			}
			item["request"] = request
			pool, err := store.GetPool(ctx, app, row.PoolID)
			if err != nil {
				deliveryFailure(c.Writer, err)
				return
			}
			report, err := store.PoolReport(ctx, app, pool)
			if err != nil {
				deliveryFailure(c.Writer, err)
				return
			}
			item["appleReport"] = report
		}
		out = append(out, item)
	}
	status, err := store.ReportStatus(ctx, app)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	if source, ok := s.offers[app].(offerdelivery.ReportSource); !ok || !source.ReportsConfigured() {
		status.State = "not_configured"
	}
	writeJSON(c.Writer, 200, map[string]any{"deliveries": out, "nextCursor": next, "reportSync": status})
}

func (s *Server) CreatePersonalDelivery(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, _ adminhttpapi.CreatePersonalDeliveryParams) {
	actor, ok := s.deliveryAccess(c, app, true, true)
	if !ok {
		return
	}
	if !s.config.WritesEnabled {
		writeFailure(c.Writer, 403, "writes_disabled", "Apple 写操作尚未启用")
		return
	}
	var in struct{ Action, RequestKey, Name, Expiration string }
	if !decodeJSON(c.Writer, c.Request, &in) {
		return
	}
	if _, err := uuid.Parse(in.RequestKey); err != nil {
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	store := s.config.Delivery
	ctx := c.Request.Context()
	now := s.now()
	switch in.Action {
	case "create":
		if !validCodePoolExpiration(in.Expiration, false, now) {
			deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
			return
		}
		if _, err := store.PreparePersonal(ctx, app, offer, in.RequestKey, strings.TrimSpace(in.Name), in.Expiration, now); err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
	case "resume":
	default:
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	planned, err := store.Personal(ctx, app, in.RequestKey)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	if planned.OfferID != offer {
		deliveryFailure(c.Writer, offerdelivery.ErrNotFound)
		return
	}
	row, request, err := store.CompletePersonal(ctx, app, in.RequestKey, actor, s.offers[app], now)
	if err != nil {
		s.auth.RecordAudit(ctx, actor, app, c.Request, "offer.personal_create", "incomplete", "personal_delivery", in.RequestKey, nil)
		if errors.Is(err, appstoreconnect.ErrRejected) {
			writeFailure(c.Writer, 422, "personal_code_rejected", "Apple 拒绝创建这枚最多兑换一次的专属码。没有自动提高兑换次数，也没有重复创建。")
			return
		}
		if errors.Is(err, appstoreconnect.ErrForbidden) {
			writeFailure(c.Writer, 403, "apple_forbidden", "Apple 创建权限不足，修复权限后可继续此记录。")
			return
		}
		if errors.Is(err, offerdelivery.ErrPersonalUncertain) {
			writeFailure(c.Writer, 409, "personal_code_uncertain", "Apple 创建结果待确认。请使用此记录的“继续核对”，不会重复创建兑换码。")
			return
		}
		deliveryFailure(c.Writer, err)
		return
	}
	s.auth.RecordAudit(ctx, actor, app, c.Request, "offer.personal_create", "succeeded", "personal_delivery", row.ID, nil)
	out := map[string]any{"delivery": row, "request": request}
	if request.ClaimExpiresAt != nil && request.ClaimExpiresAt.After(now) && (request.Status == "assigned" || request.Status == "delivered") {
		out["token"] = s.deliveryToken(app, request.ID, request.ClaimGeneration)
	}
	writeJSON(c.Writer, 200, out)
}
