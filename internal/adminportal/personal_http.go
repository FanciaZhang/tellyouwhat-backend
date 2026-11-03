package adminportal

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

func (s *Server) ListPersonalDeliveries(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, _ adminhttpapi.ListPersonalDeliveriesParams) {
	if _, ok := s.deliveryAccess(c, app, false, false); !ok {
		return
	}
	ctx := c.Request.Context()
	rows, next, err := s.config.Delivery.ListPersonal(ctx, app, offer, c.Query("cursor"))
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	out := []any{}
	for _, row := range rows {
		request, err := s.config.Delivery.Get(ctx, app, row.RequestID)
		if err != nil {
			deliveryFailure(c.Writer, err)
			return
		}
		// Offer-wide Apple counts cannot establish redemption of an individual one-time code.
		out = append(out, map[string]any{"delivery": row, "request": request})
	}
	pools, err := s.personalPools(ctx, app, offer)
	inventoryError := ""
	if err != nil {
		pools = []personalPool{}
		inventoryError = "暂时无法核对 Apple 库存。历史发放仍可查看，恢复后可继续分配。"
	}
	writeJSON(c.Writer, 200, map[string]any{"deliveries": out, "nextCursor": next, "pools": pools, "inventoryError": inventoryError})
}

type personalPool struct {
	ID          string `json:"id"`
	Expiration  string `json:"expiration"`
	Capacity    int    `json:"capacity"`
	Available   int    `json:"available"`
	Unconfirmed int    `json:"unconfirmed"`
	Managed     bool   `json:"managed"`
	Active      bool   `json:"active"`
}

func (s *Server) personalPools(ctx context.Context, app, offer string) ([]personalPool, error) {
	manager := s.offers[app]
	offers, err := manager.ListOffers(ctx)
	if err != nil {
		return nil, err
	}
	found, active := false, false
	for _, o := range offers {
		if o.ID == offer {
			found, active = true, o.Active
			break
		}
	}
	if !found {
		return nil, offerdelivery.ErrNotFound
	}
	pools, err := manager.ListCodePools(ctx, offer)
	if err != nil {
		return nil, err
	}
	out := []personalPool{}
	for _, p := range pools {
		if p.Kind != "oneTime" || p.Environment != "PRODUCTION" {
			continue
		}
		expiry, err := offerdelivery.AppleCodeExpiry(p.ExpirationDate)
		if err != nil {
			return nil, err
		}
		v := personalPool{ID: p.ID, Expiration: p.ExpirationDate, Capacity: p.NumberOfCodes, Active: active && p.Active && expiry.After(s.now())}
		stored, err := s.config.Delivery.GetPool(ctx, app, p.ID)
		if err != nil && !errors.Is(err, offerdelivery.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			if stored.OfferID != offer || stored.Kind != "oneTime" || stored.Environment != "production" {
				return nil, offerdelivery.ErrConflict
			}
			summary, err := s.config.Delivery.Summary(ctx, app, p.ID)
			if err != nil {
				return nil, err
			}
			v.Managed = summary.Imported > 0
			if v.Active {
				v.Available = summary.Available
				v.Unconfirmed = summary.External + p.NumberOfCodes - summary.Imported
			}
		}
		if !v.Managed && v.Active {
			v.Unconfirmed = p.NumberOfCodes
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) CreatePersonalDelivery(c *gin.Context, app adminhttpapi.AppID, offer adminhttpapi.OfferID, _ adminhttpapi.CreatePersonalDeliveryParams) {
	actor, ok := s.deliveryAccess(c, app, true, true)
	if !ok {
		return
	}
	var in struct {
		Action, RequestKey, Name, PoolID string
		ConfirmUnissued                  bool
	}
	if !decodeJSON(c.Writer, c.Request, &in) {
		return
	}
	if _, err := uuid.Parse(in.RequestKey); err != nil {
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	if in.Action != "create" && in.Action != "resume" {
		deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	ctx := c.Request.Context()
	store := s.config.Delivery
	row, err := store.Personal(ctx, app, in.RequestKey)
	var request offerdelivery.Request
	if err == nil {
		if row.OfferID != offer {
			deliveryFailure(c.Writer, offerdelivery.ErrNotFound)
			return
		}
		if in.Action == "create" && (row.PoolID != in.PoolID || row.Name != in.Name) {
			deliveryFailure(c.Writer, offerdelivery.ErrConflict)
			return
		}
		request, err = store.Get(ctx, app, row.RequestID)
	} else if errors.Is(err, offerdelivery.ErrNotFound) && in.Action == "create" {
		if !(offerdelivery.Recipient{Name: in.Name}).Valid() || cleanID(in.PoolID) == "" {
			deliveryFailure(c.Writer, offerdelivery.ErrInvalid)
			return
		}
		var pool offerdelivery.Pool
		pool, err = s.syncDeliveryPool(ctx, app, offer, in.PoolID)
		if err == nil && (pool.Kind != "oneTime" || pool.Environment != "production") {
			err = offerdelivery.ErrUnavailable
		}
		if err == nil {
			if in.ConfirmUnissued {
				var raw []byte
				raw, err = s.offers[app].DownloadOneTimeCodes(ctx, pool.ID)
				if err == nil {
					var codes []string
					codes, err = offerdelivery.ParseCSV(raw, pool.Capacity)
					if err == nil {
						row, request, err = store.IssuePersonalWithUnissuedCodes(ctx, app, offer, pool.ID, in.RequestKey, in.Name, actor, codes, s.now())
					}
				}
			} else {
				row, request, err = store.IssuePersonal(ctx, app, offer, in.PoolID, in.RequestKey, in.Name, actor, s.now())
			}
		}
	}
	if err != nil {
		deliveryFailure(c.Writer, err)
		return
	}
	s.auth.RecordAudit(ctx, actor, app, c.Request, "offer.personal_allocate", "succeeded", "personal_delivery", row.ID, nil)
	out := map[string]any{"delivery": row, "request": request}
	if request.ClaimExpiresAt != nil && request.ClaimExpiresAt.After(s.now()) && (request.Status == "assigned" || request.Status == "delivered") {
		out["token"] = s.deliveryToken(app, request.ID, request.ClaimGeneration)
	}
	writeJSON(c.Writer, 200, out)
}
