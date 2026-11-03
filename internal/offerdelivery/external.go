package offerdelivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RecordExternal records a past delivery without exposing or allocating a new code.
// Historical deliveries may belong to an inactive pool; inventory is never returned.
func (s Store) RecordExternal(ctx context.Context, app, pool, key, actor string, recipient Recipient, code string, deliveredAt, now time.Time) (Request, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	deliveredAt = deliveredAt.UTC().Truncate(time.Microsecond)
	if !validApp(app) || !idPattern.MatchString(key) || actor == "" || !recipient.Valid() || deliveredAt.IsZero() || deliveredAt.After(now) {
		return Request{}, ErrInvalid
	}
	raw, err := json.Marshal(recipient)
	if err != nil {
		return Request{}, err
	}
	input, _ := json.Marshal(struct {
		Pool, Code  string
		Recipient   Recipient
		DeliveredAt time.Time
	}{pool, code, recipient, deliveredAt})
	hash := sha256.Sum256(input)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, pool)
	if err != nil {
		return Request{}, err
	}
	var existing string
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM offer_delivery_requests WHERE app_id=? AND request_key=?`, app, key).Scan(&existing, &previous)
	if err == nil {
		if string(previous) != string(hash[:]) {
			return Request{}, ErrConflict
		}
		tx.Rollback()
		return s.Get(ctx, app, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Request{}, err
	}
	var codeID any
	if p.Kind == "oneTime" {
		if !codePattern.MatchString(code) {
			return Request{}, ErrInvalid
		}
		digest := sha256.Sum256([]byte(code))
		var id, state string
		err = tx.QueryRowContext(ctx, `SELECT id,inventory_state FROM offer_delivery_codes WHERE app_id=? AND pool_id=? AND code_hash=? FOR UPDATE`, app, pool, digest[:]).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return Request{}, ErrNotFound
		}
		if err != nil {
			return Request{}, err
		}
		if state == "assigned" {
			return Request{}, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='assigned' WHERE app_id=? AND id=?`, app, id); err != nil {
			return Request{}, err
		}
		codeID = id
	} else {
		if code != "" {
			return Request{}, ErrInvalid
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM offer_delivery_requests WHERE app_id=? AND pool_id=? AND assigned_at IS NOT NULL`, app, pool).Scan(&n); err != nil {
			return Request{}, err
		}
		if n >= p.Capacity {
			return Request{}, ErrUnavailable
		}
	}
	id := uuid.NewString()
	encrypted, nonce, err := s.Cipher.Encrypt(raw, aad(app, "recipient", id))
	if err != nil {
		return Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_requests(app_id,id,pool_id,request_key,request_hash,ciphertext,nonce,status,source,code_id,requested_at,assigned_at,delivered_at) VALUES(?,?,?,?,?,?,?,'delivered','external',?,?,?,?)`, app, id, pool, key, hash[:], encrypted, nonce, codeID, now.UTC(), deliveredAt, deliveredAt); err != nil {
		return Request{}, err
	}
	if err = event(ctx, tx, app, id, actor, "request.record_external", 1, now); err != nil {
		return Request{}, err
	}
	if err = tx.Commit(); err != nil {
		return Request{}, err
	}
	return s.Get(ctx, app, id)
}
