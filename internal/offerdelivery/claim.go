package offerdelivery

import (
	"context"
	"time"
)

// SetClaimLink exposes only the already allocated code through a revocable recipient link.
func (s Store) SetClaimLink(ctx context.Context, app, id, actor string, version int, enabled bool, now time.Time) (Request, error) {
	if !validApp(app) || actor == "" {
		return Request{}, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback()
	r, err := s.scan(app, tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND id=? FOR UPDATE`, app, id))
	if err != nil {
		return r, err
	}
	if r.Version != version || (r.Status != "assigned" && r.Status != "delivered") {
		return r, ErrConflict
	}
	action := "request.share_claim"
	if enabled {
		if r.ClaimExpiresAt == nil || !r.ClaimExpiresAt.After(now) {
			_, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET claim_generation=claim_generation+1,claim_expires_at=?,version=version+1 WHERE app_id=? AND id=?`, now.Add(90*24*time.Hour).UTC(), app, id)
		}
	} else {
		action = "request.revoke_claim"
		_, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET claim_generation=claim_generation+1,claim_expires_at=NULL,version=version+1 WHERE app_id=? AND id=?`, app, id)
	}
	if err != nil {
		return r, err
	}
	if err = event(ctx, tx, app, id, actor, action, 1, now); err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return s.Get(ctx, app, id)
}

// AccessClaim serializes token revocation with code disclosure and delivery status.
func (s Store) AccessClaim(ctx context.Context, app, id string, generation int, action string, now time.Time) (Request, string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, "", err
	}
	defer tx.Rollback()
	r, err := s.scan(app, tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND id=? FOR UPDATE`, app, id))
	if err != nil {
		return r, "", err
	}
	if r.ClaimGeneration != generation || r.ClaimExpiresAt == nil || !r.ClaimExpiresAt.After(now) {
		return r, "", ErrNotFound
	}
	code := ""
	switch action {
	case "status":
	case "claim":
		if r.Status != "assigned" && r.Status != "delivered" {
			return r, "", ErrConflict
		}
		var active bool
		var expires time.Time
		if err = tx.QueryRowContext(ctx, `SELECT active,expires_at FROM offer_delivery_pools WHERE app_id=? AND pool_id=?`, app, r.PoolID).Scan(&active, &expires); err != nil {
			return r, "", err
		}
		if !active || !expires.After(now) {
			return r, "", ErrUnavailable
		}
		var raw, nonce []byte
		kind, contextID := "code", r.CodeID
		if r.CodeID != "" {
			err = tx.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM offer_delivery_codes WHERE app_id=? AND id=?`, app, r.CodeID).Scan(&raw, &nonce)
		} else {
			kind, contextID = "pool", r.PoolID
			err = tx.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM offer_delivery_pools WHERE app_id=? AND pool_id=? AND kind='custom'`, app, r.PoolID).Scan(&raw, &nonce)
		}
		if err != nil {
			return r, "", err
		}
		raw, err = s.Cipher.Decrypt(raw, nonce, aad(app, kind, contextID))
		if err != nil {
			return r, "", err
		}
		code = string(raw)
		if r.ClaimedAt == nil {
			if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET status='delivered',delivered_at=COALESCE(delivered_at,?),claimed_at=?,version=version+1 WHERE app_id=? AND id=?`, now.UTC(), now.UTC(), app, id); err != nil {
				return r, "", err
			}
			if r.DeliveredAt == nil {
				r.DeliveredAt = &now
			}
			r.ClaimedAt = &now
			r.Status = "delivered"
			r.Version++
			if err = event(ctx, tx, app, id, "link_holder", "request.claim", 1, now); err != nil {
				return r, "", err
			}
		}
		if err = event(ctx, tx, app, id, "link_holder", "code.reveal", 1, now); err != nil {
			return r, "", err
		}
	case "feedback":
		if r.Status != "delivered" {
			return r, "", ErrConflict
		}
		if r.ReportedRedeemedAt == nil {
			if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET reported_redeemed_at=?,version=version+1 WHERE app_id=? AND id=?`, now.UTC(), app, id); err != nil {
				return r, "", err
			}
			r.ReportedRedeemedAt = &now
			r.Version++
			if err = event(ctx, tx, app, id, "link_holder", "request.report_redeemed", 1, now); err != nil {
				return r, "", err
			}
		}
	default:
		return r, "", ErrInvalid
	}
	if err = tx.Commit(); err != nil {
		return r, "", err
	}
	return r, code, nil
}
