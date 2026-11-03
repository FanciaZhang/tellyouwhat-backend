// Package purchase describes metadata extracted from verified Apple transactions.
package purchase

import "time"

// Evidence is absent for legacy transactions whose payment metadata was not collected.
// PriceMilli is the transaction price in thousandths of Currency, not developer proceeds.
type Evidence struct {
	OwnershipType string
	TransactionID string
	OriginalID    string
	PriceMilli    *int64
	Currency      string
	PurchasedAt   time.Time
	StartedAt     time.Time
	SignedAt      time.Time
	RevokedAt     *time.Time
}

func (e Evidence) HasPrice() bool {
	return e.OwnershipType != "FAMILY_SHARED" && e.PriceMilli != nil && *e.PriceMilli >= 0 && len(e.Currency) == 3 && !e.PurchasedAt.IsZero()
}
