package adminportal

import (
	"context"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
	"strings"
)

// prepareFreshBatch runs only after Apple returns a successful creation response.
// It uses that response directly, because Apple's listing may lag behind creation.
func (s *Server) prepareFreshBatch(ctx context.Context, app, offer, actor string, batch appstoreconnect.CodePool) error {
	manager := s.offers[app]
	offers, err := manager.ListOffers(ctx)
	if err != nil {
		return err
	}
	for _, o := range offers {
		if o.ID != offer {
			continue
		}
		expiry, err := offerdelivery.AppleCodeExpiry(batch.ExpirationDate)
		if err != nil {
			return err
		}
		p := offerdelivery.Pool{ID: batch.ID, OfferID: offer, OfferName: o.Name, SubscriptionID: o.SubscriptionID, ProductID: o.ProductID, Kind: batch.Kind, Environment: strings.ToLower(batch.Environment), Capacity: batch.NumberOfCodes, Active: o.Active && batch.Active, ExpiresAt: expiry, SyncedAt: s.now()}
		if err = s.config.Delivery.SyncPool(ctx, app, p); err != nil {
			return err
		}
		raw, err := manager.DownloadOneTimeCodes(ctx, batch.ID)
		if err != nil {
			return err
		}
		codes, err := offerdelivery.ParseCSV(raw, p.Capacity)
		if err != nil {
			return err
		}
		return s.config.Delivery.ImportFreshCodes(ctx, app, p.ID, actor, codes, s.now())
	}
	return offerdelivery.ErrNotFound
}
