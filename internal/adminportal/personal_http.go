package adminportal

import (
	"github.com/gin-gonic/gin"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
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

// Single-redemption custom codes are not supported by Apple's minimum batch quota.
func (s *Server) CreatePersonalDelivery(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, _ adminhttpapi.CreatePersonalDeliveryParams) {
	if _, ok := s.deliveryAccess(c, app, true, true); !ok {
		return
	}
	if !s.config.WritesEnabled {
		writeFailure(c.Writer, 403, "writes_disabled", "Apple 写操作尚未启用")
		return
	}
	writeFailure(c.Writer, 410, "personal_code_unsupported", "Apple 自定义码最低为 500 次兑换额度，不支持创建限兑一次的专属码。此入口已关闭。")
}
