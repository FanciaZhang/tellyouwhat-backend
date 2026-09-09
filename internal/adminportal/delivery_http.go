package adminportal

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

func deliveryFailure(w http.ResponseWriter, err error) {
	var duplicate *mysql.MySQLError
	switch {
	case errors.Is(err, offerdelivery.ErrNotFound):
		writeFailure(w, 404, "delivery_not_found", "未找到这条记录或匹配的核销证据")
	case errors.Is(err, offerdelivery.ErrInvalid):
		writeFailure(w, 422, "invalid_delivery", "请检查填写内容和完整码池数据")
	case errors.Is(err, offerdelivery.ErrConflict), errors.As(err, &duplicate) && duplicate.Number == 1062:
		writeFailure(w, 409, "delivery_conflict", "记录已变化或已被关联，请刷新后再操作")
	case errors.Is(err, offerdelivery.ErrUnavailable):
		writeFailure(w, 409, "inventory_unavailable", "没有确认可分配的库存，或 Offer 已停用、过期")
	default:
		writeFailure(w, 503, "delivery_unavailable", "发放台账暂时不可用，请刷新记录确认操作结果后再试")
	}
}

// syncDeliveryPool verifies the entire App -> Offer -> pool chain before storing metadata.
func (s *Server) syncDeliveryPool(ctx context.Context, app, offer, pool string) (offerdelivery.Pool, error) {
	manager := s.offers[app]
	if manager == nil || cleanID(offer) == "" || cleanID(pool) == "" {
		return offerdelivery.Pool{}, offerdelivery.ErrNotFound
	}
	offers, err := manager.ListOffers(ctx)
	if err != nil {
		return offerdelivery.Pool{}, err
	}
	name, subscriptionID, productID := "", "", ""
	active := false
	for _, o := range offers {
		if o.ID == offer {
			name, subscriptionID, productID = o.Name, o.SubscriptionID, o.ProductID
			active = o.Active
			break
		}
	}
	if name == "" {
		return offerdelivery.Pool{}, offerdelivery.ErrNotFound
	}
	pools, err := manager.ListCodePools(ctx, offer)
	if err != nil {
		return offerdelivery.Pool{}, err
	}
	for _, v := range pools {
		if v.ID == pool {
			// Apple custom pools may have no expiry; represent this as an unbounded date.
			expiry := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
			if v.ExpirationDate != "" {
				expiry, err = time.Parse("2006-01-02", v.ExpirationDate)
				if err != nil {
					return offerdelivery.Pool{}, offerdelivery.ErrInvalid
				}
			}
			env := strings.ToLower(v.Environment)
			if env == "" && v.Kind == "custom" {
				env = "production"
			}
			p := offerdelivery.Pool{ID: v.ID, OfferID: offer, OfferName: name, SubscriptionID: subscriptionID, ProductID: productID, Code: v.Code, Kind: v.Kind, Environment: env, Capacity: v.NumberOfCodes, Active: active && v.Active, ExpiresAt: expiry, SyncedAt: s.now()}
			return p, s.config.Delivery.SyncPool(ctx, app, p)
		}
	}
	return offerdelivery.Pool{}, offerdelivery.ErrNotFound
}

func (s *Server) deliveryAccess(c *gin.Context, app string, write, recent bool) (string, bool) {
	c.Header("Cache-Control", "no-store")
	if _, _, ok := s.offerManager(c.Writer, app); !ok {
		return "", false
	}
	permission := adminauth.PermissionOfferRead
	if write {
		permission = adminauth.PermissionOfferManage
	}
	auth, ok := s.auth.RequirePermission(c.Writer, c.Request, permission, app, write, recent)
	if !ok {
		return "", false
	}
	if s.config.Delivery == nil {
		writeFailure(c.Writer, 503, "delivery_unavailable", "发放台账尚未启用")
		return "", false
	}
	return auth.User.ID, true
}
func (s *Server) GetOfferDelivery(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, pool string) {
	s.readOfferDelivery(c, app, offer, pool, "", "", c.Query("cursor"))
}
func (s *Server) readOfferDelivery(c *gin.Context, app, offer, pool, query, status, cursor string) {
	if _, ok := s.deliveryAccess(c, app, false, false); !ok {
		return
	}
	p, err := s.config.Delivery.GetPool(c.Request.Context(), app, pool)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	if p.OfferID != offer {
		deliveryFailure(c.Writer, offerdelivery.ErrNotFound)
		return
	}
	summary, err := s.config.Delivery.Summary(c.Request.Context(), app, pool)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	page, err := s.config.Delivery.Search(c.Request.Context(), app, pool, cursor, query, status)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	events, err := s.config.Delivery.Events(c.Request.Context(), app, pool)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	verified, err := s.config.Delivery.VerifiedSubscriptions(c.Request.Context(), app, pool)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	observed, err := s.config.Delivery.ObservedOfferCount(c.Request.Context(), app, p)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	unknownProduct, err := s.config.Delivery.UnknownProductOfferCount(c.Request.Context(), app, p)
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	moreVerified := len(verified) > 100
	if moreVerified {
		verified = verified[:100]
	}
	writeJSON(c.Writer, 200, map[string]any{"pool": p, "summary": summary, "observedOfferSubscriptions": observed, "unknownProductSubscriptions": unknownProduct, "requests": page.Requests, "nextCursor": page.NextCursor, "events": events, "verifiedSubscriptions": verified, "moreVerified": moreVerified, "stale": s.now().Sub(p.SyncedAt) > 5*time.Minute})
}

type deliveryCommand struct {
	Query         string                  `json:"query"`
	Status        string                  `json:"status"`
	Cursor        string                  `json:"cursor"`
	Code          string                  `json:"code"`
	DeliveredAt   time.Time               `json:"deliveredAt"`
	Action        string                  `json:"action"`
	RequestID     string                  `json:"requestID"`
	Version       int                     `json:"version"`
	RequestKey    string                  `json:"requestKey"`
	ExpectedCount int                     `json:"expectedCount"`
	Reference     string                  `json:"reference"`
	Recipient     offerdelivery.Recipient `json:"recipient"`
}

func (s *Server) CommandOfferDelivery(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, pool string, _ adminhttpapi.CommandOfferDeliveryParams) {
	if _, ok := s.deliveryAccess(c, app, false, false); !ok {
		return
	}
	var in deliveryCommand
	if !decodeJSON(c.Writer, c.Request, &in) {
		return
	}
	if in.Action == "search" {
		if _, ok := s.auth.RequirePermission(c.Writer, c.Request, adminauth.PermissionOfferRead, app, true, false); !ok {
			return
		}
		s.readOfferDelivery(c, app, offer, pool, in.Query, in.Status, in.Cursor)
		return
	}
	recent := in.Action == "reveal" || in.Action == "import" || in.Action == "confirm_inventory" || in.Action == "link_verified" || in.Action == "record_external" || in.Action == "share_claim" || in.Action == "revoke_claim"
	actor, ok := s.deliveryAccess(c, app, true, recent)
	if !ok {
		return
	}
	store := s.config.Delivery
	ctx := c.Request.Context()
	now := s.now()
	var p offerdelivery.Pool
	var err error
	switch in.Action {
	case "sync", "import", "confirm_inventory", "request", "assign":
		p, err = s.syncDeliveryPool(ctx, app, offer, pool)
	default:
		p, err = store.GetPool(ctx, app, pool)
	}
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	if p.OfferID != offer {
		deliveryFailure(c.Writer, offerdelivery.ErrNotFound)
		return
	}
	var result any = map[string]bool{"ok": true}
	switch in.Action {
	case "sync":
	case "import":
		if p.Kind != "oneTime" {
			deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
			return
		}
		var raw []byte
		raw, err = s.offers[app].DownloadOneTimeCodes(ctx, pool)
		if err == nil {
			var codes []string
			codes, err = offerdelivery.ParseCSV(raw, p.Capacity)
			if err == nil {
				err = store.ImportCodes(ctx, app, pool, actor, codes, now)
			}
		}
	case "confirm_inventory":
		err = store.ConfirmExternalInventory(ctx, app, pool, actor, in.ExpectedCount, now)
	case "record_external":
		if !idempotencyPattern.MatchString(in.RequestKey) {
			deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
			return
		}
		result, err = store.RecordExternal(ctx, app, pool, in.RequestKey, actor, in.Recipient, in.Code, in.DeliveredAt, now)
	case "request":
		if !idempotencyPattern.MatchString(in.RequestKey) {
			deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
			return
		}
		result, err = store.CreateRequest(ctx, app, pool, in.RequestKey, actor, in.Recipient, now)
	case "assign", "reveal", "deliver", "cancel", "reject", "report_redeemed", "link_verified", "share_claim", "revoke_claim":
		var r offerdelivery.Request
		r, err = store.Get(ctx, app, in.RequestID)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		if r.PoolID != pool {
			deliveryFailure(c.Writer, offerdelivery.ErrNotFound)
			return
		}
		switch in.Action {
		case "share_claim", "revoke_claim":
			r, err = store.SetClaimLink(ctx, app, in.RequestID, actor, in.Version, in.Action == "share_claim", now)
			result = map[string]any{"request": r}
			if err == nil && in.Action == "share_claim" {
				result = map[string]any{"request": r, "token": s.deliveryToken(app, r.ID, r.ClaimGeneration)}
			}
		case "assign":
			result, err = store.Assign(ctx, app, in.RequestID, actor, in.Version, now)
		case "reveal":
			var code string
			code, err = store.Reveal(ctx, app, in.RequestID, actor, now)
			result = map[string]string{"code": code}
		case "link_verified":
			result, err = store.LinkVerified(ctx, app, in.RequestID, actor, in.Reference, in.Version, now)
		default:
			result, err = store.Transition(ctx, app, in.RequestID, actor, in.Action, in.Version, now)
		}
	default:
		err = offerdelivery.ErrInvalid
	}
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	writeJSON(c.Writer, 200, result)
}
