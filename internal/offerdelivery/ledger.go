package offerdelivery

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s Store) GetPool(ctx context.Context, app, id string) (Pool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Pool{}, err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, id)
	if err != nil {
		return p, err
	}
	return p, tx.Commit()
}

type Page struct {
	Requests   []Request `json:"requests"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// Search decrypts bounded pages in memory; recipient values never enter SQL or logs.
func (s Store) Search(ctx context.Context, app, pool, cursor, query, status string) (Page, error) {
	out := Page{Requests: []Request{}}
	if len(query) > 500 || len(cursor) > 40 {
		return out, ErrInvalid
	}
	if status != "" && status != "requested" && status != "assigned" && status != "delivered" && status != "cancelled" && status != "rejected" {
		return out, ErrInvalid
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND pool_id=? AND id>? AND (?='' OR status=?) ORDER BY id LIMIT 2001`, app, pool, cursor, status, status)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	query = strings.ToLower(strings.TrimSpace(query))
	scanned := 0
	last := cursor
	for rows.Next() {
		r, err := s.scan(app, rows)
		if err != nil {
			return out, err
		}
		if len(out.Requests) == 100 || scanned == 2000 {
			out.NextCursor = last
			break
		}
		last = r.ID
		scanned++
		v := r.Recipient
		if query == "" || strings.Contains(strings.ToLower(strings.Join([]string{v.Name, v.Contact, v.Reference, v.Channel, v.Note}, "\n")), query) {
			out.Requests = append(out.Requests, r)
		}
	}
	return out, rows.Err()
}

// ConfirmExternalInventory is an explicit operator assertion, never inferred from missing redemptions.
func (s Store) ConfirmExternalInventory(ctx context.Context, app, pool, actor string, expected int, now time.Time) error {
	if actor == "" || expected < 1 {
		return ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, pool)
	if err != nil {
		return err
	}
	if p.Kind != "oneTime" || !p.Active || !p.ExpiresAt.After(now) {
		return ErrUnavailable
	}
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM offer_delivery_codes WHERE app_id=? AND pool_id=? AND inventory_state='external'`, app, pool).Scan(&n); err != nil {
		return err
	}
	if n != expected {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='available' WHERE app_id=? AND pool_id=? AND inventory_state='external'`, app, pool); err != nil {
		return err
	}
	if err = event(ctx, tx, app, pool, actor, "inventory.confirm_available", n, now); err != nil {
		return err
	}
	return tx.Commit()
}

type Event struct {
	ID        int64     `json:"id"`
	TargetID  string    `json:"targetID"`
	ActorID   string    `json:"actorID"`
	Action    string    `json:"action"`
	Quantity  int       `json:"quantity"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s Store) Events(ctx context.Context, app, pool string) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id,e.target_id,e.actor_id,e.action,e.quantity,e.created_at FROM offer_delivery_events e WHERE e.app_id=? AND (e.target_id=? OR EXISTS(SELECT 1 FROM offer_delivery_requests r WHERE r.app_id=e.app_id AND r.pool_id=? AND r.id=e.target_id)) ORDER BY e.id DESC LIMIT 100`, app, pool, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.TargetID, &e.ActorID, &e.Action, &e.Quantity, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type VerifiedSubscription struct {
	Reference       string    `json:"reference"`
	FirstObservedAt time.Time `json:"firstObservedAt"`
	LinkedRequestID string    `json:"linkedRequestID,omitempty"`
}

func (s Store) VerifiedSubscriptions(ctx context.Context, app, pool string) ([]VerifiedSubscription, error) {
	p, err := s.GetPool(ctx, app, pool)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT LOWER(HEX(r.original_transaction_hash)),MIN(r.redeemed_at),COALESCE(MAX(l.request_id),'') FROM app_store_offer_redemptions r LEFT JOIN offer_delivery_verified_links l ON l.app_id=r.app_id AND l.environment=r.environment AND BINARY l.offer_name=BINARY r.offer_identifier AND l.original_hash=r.original_transaction_hash WHERE r.app_id=? AND r.environment=? AND BINARY r.offer_identifier=BINARY ? AND r.product_id=? AND r.offer_type=3 GROUP BY r.original_transaction_hash ORDER BY MIN(r.redeemed_at) DESC LIMIT 101`, app, p.Environment, p.OfferName, p.ProductID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerifiedSubscription{}
	for rows.Next() {
		var v VerifiedSubscription
		if err = rows.Scan(&v.Reference, &v.FirstObservedAt, &v.LinkedRequestID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ManagedExport records exposure for existing managed pools; untracked pools have no inventory to release.
func (s Store) ManagedExport(ctx context.Context, app, pool, actor string, now time.Time) error {
	err := s.RecordExport(ctx, app, pool, actor, now)
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// ObservedOfferCount covers every pool of the same Offer; Apple does not identify one-time pools.
func (s Store) ObservedOfferCount(ctx context.Context, app string, p Pool) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(DISTINCT original_transaction_hash) FROM app_store_offer_redemptions WHERE app_id=? AND environment=? AND BINARY offer_identifier=BINARY ? AND product_id=? AND offer_type=3`, app, p.Environment, p.OfferName, p.ProductID).Scan(&count)
	return count, err
}

// OfferSummary is operational ledger coverage, not Apple's remaining inventory.
type OfferSummary struct {
	OfferID            string `json:"offerID"`
	Environment        string `json:"environment"`
	Pools              int    `json:"pools"`
	Applications       int    `json:"applications"`
	Delivered          int    `json:"delivered"`
	Pending            int    `json:"pending"`
	ExternalDeliveries int    `json:"externalDeliveries"`
}

func (s Store) OfferSummaries(ctx context.Context, app string) ([]OfferSummary, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.offer_id,p.environment,COUNT(DISTINCT p.pool_id),COALESCE(SUM(r.source<>'external'),0),COUNT(r.delivered_at),COALESCE(SUM(r.status='requested'),0),COALESCE(SUM(r.source='external'),0) FROM offer_delivery_pools p LEFT JOIN offer_delivery_requests r ON r.app_id=p.app_id AND r.pool_id=p.pool_id WHERE p.app_id=? GROUP BY p.offer_id,p.environment`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OfferSummary{}
	for rows.Next() {
		var v OfferSummary
		if err = rows.Scan(&v.OfferID, &v.Environment, &v.Pools, &v.Applications, &v.Delivered, &v.Pending, &v.ExternalDeliveries); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UnknownProductOfferCount preserves legacy evidence without assigning it to a product.
func (s Store) UnknownProductOfferCount(ctx context.Context, app string, p Pool) (int, error) {
	if p.ProductID == "" {
		return 0, nil
	}
	p.ProductID = ""
	return s.ObservedOfferCount(ctx, app, p)
}
