package offerdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
)

type PersonalDelivery struct {
	ID         string    `json:"id"`
	OfferID    string    `json:"offerID"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	PoolID     string    `json:"poolID,omitempty"`
	RequestID  string    `json:"requestID,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Expiration string    `json:"expiration"`
}

func (s Store) Personal(ctx context.Context, app, id string) (PersonalDelivery, error) {
	var v PersonalDelivery
	var encrypted, nonce []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id,offer_id,state,COALESCE(pool_id,''),COALESCE(request_id,''),created_at,ciphertext,nonce FROM offer_personal_deliveries WHERE app_id=? AND id=?`, app, id).Scan(&v.ID, &v.OfferID, &v.State, &v.PoolID, &v.RequestID, &v.CreatedAt, &encrypted, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	raw, err := s.Cipher.Decrypt(encrypted, nonce, aad(app, "personal", id))
	if err != nil {
		return v, err
	}
	var secret struct{ Name, Expiration string }
	if err = json.Unmarshal(raw, &secret); err != nil {
		return v, err
	}
	v.Name, v.Expiration = secret.Name, secret.Expiration
	return v, nil
}

// IssuePersonal atomically reserves one confirmed one-time code and its claim link.
// It never creates codes at Apple, changes their expiry, or releases an assigned code.
func (s Store) IssuePersonal(ctx context.Context, app, offer, pool, id, name, actor string, now time.Time) (PersonalDelivery, Request, error) {
	var v PersonalDelivery
	var r Request
	name = strings.TrimSpace(name)
	if _, err := uuid.Parse(id); err != nil {
		return v, r, ErrInvalid
	}
	if !validApp(app) || !idPattern.MatchString(offer) || !idPattern.MatchString(pool) || !(Recipient{Name: name}).Valid() || actor == "" {
		return v, r, ErrInvalid
	}
	rawInput, _ := json.Marshal([]string{offer, pool, name})
	inputHash := sha256.Sum256(rawInput)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return v, r, err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, pool)
	if err != nil {
		return v, r, err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT input_hash FROM offer_personal_deliveries WHERE app_id=? AND id=?`, app, id).Scan(&previous)
	if err == nil {
		if !bytes.Equal(previous, inputHash[:]) {
			return v, r, ErrConflict
		}
		tx.Rollback()
		v, err = s.Personal(ctx, app, id)
		if err != nil {
			return v, r, err
		}
		r, err = s.Get(ctx, app, v.RequestID)
		return v, r, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return v, r, err
	}
	if p.OfferID != offer {
		return v, r, ErrNotFound
	}
	if p.Kind != "oneTime" || p.Environment != "production" || !p.Active || !p.ExpiresAt.After(now) {
		return v, r, ErrUnavailable
	}
	var codeID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM offer_delivery_codes WHERE app_id=? AND pool_id=? AND inventory_state='available' ORDER BY id LIMIT 1 FOR UPDATE`, app, pool).Scan(&codeID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, r, ErrUnavailable
	}
	if err != nil {
		return v, r, err
	}
	requestID := uuid.NewString()
	recipient := Recipient{Name: name, Channel: "熟人发放"}
	rawRecipient, _ := json.Marshal(recipient)
	requestHash := sha256.Sum256(append([]byte("personal:"+pool+":"), rawRecipient...))
	encrypted, nonce, err := s.Cipher.Encrypt(rawRecipient, aad(app, "recipient", requestID))
	if err != nil {
		return v, r, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_requests(app_id,id,pool_id,request_key,request_hash,ciphertext,nonce,source,status,code_id,requested_at,assigned_at,claim_generation,claim_expires_at) VALUES(?,?,?,?,?,?,?,'personal','assigned',?,?,?,1,?)`, app, requestID, pool, "personal:"+id, requestHash[:], encrypted, nonce, codeID, now.UTC(), now.UTC(), now.Add(90*24*time.Hour).UTC())
	if err != nil {
		return v, r, err
	}
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return v, r, err
	}
	secret, _ := json.Marshal(struct{ Name, Expiration string }{name, p.ExpiresAt.In(zone).Format("2006-01-02")})
	encrypted, nonce, err = s.Cipher.Encrypt(secret, aad(app, "personal", id))
	if err != nil {
		return v, r, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO offer_personal_deliveries(app_id,id,offer_id,input_hash,ciphertext,nonce,state,pool_id,request_id,created_at) VALUES(?,?,?,?,?,?,'ready',?,?,?)`, app, id, offer, inputHash[:], encrypted, nonce, pool, requestID, now.UTC())
	if err != nil {
		return v, r, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='assigned' WHERE app_id=? AND id=?`, app, codeID); err != nil {
		return v, r, err
	}
	for _, action := range []string{"request.personal_create", "request.assign", "request.share_claim"} {
		if err = event(ctx, tx, app, requestID, actor, action, 1, now); err != nil {
			return v, r, err
		}
	}
	if err = tx.Commit(); err != nil {
		return v, r, err
	}
	v, err = s.Personal(ctx, app, id)
	if err != nil {
		return v, r, err
	}
	r, err = s.Get(ctx, app, requestID)
	return v, r, err
}

// Apple expires offer codes at midnight Pacific time, including daylight saving.
func AppleCodeExpiry(day string) (time.Time, error) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.Time{}, err
	}
	value, err := time.ParseInLocation("2006-01-02", day, zone)
	if err != nil {
		return time.Time{}, ErrInvalid
	}
	return value.UTC(), nil
}

func (s Store) ListPersonal(ctx context.Context, app, offer, cursor string) ([]PersonalDelivery, string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM offer_personal_deliveries WHERE app_id=? AND offer_id=? AND id>? ORDER BY id LIMIT 21`, app, offer, cursor)
	if err != nil {
		return nil, "", err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > 20 {
		ids = ids[:20]
		next = ids[19]
	}
	out := []PersonalDelivery{}
	for _, id := range ids {
		v, err := s.Personal(ctx, app, id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	return out, next, nil
}
