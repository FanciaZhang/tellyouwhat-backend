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

type Link struct {
	ID              string     `json:"id"`
	PoolID          string     `json:"poolID"`
	Label           string     `json:"label"`
	MaxApplications int        `json:"maxApplications"`
	Applications    int        `json:"applications"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
}

func (s Store) CreateLink(ctx context.Context, app, pool, key, actor, label string, max int, expires, now time.Time) (Link, error) {
	expires = expires.UTC().Truncate(time.Microsecond)
	label = strings.TrimSpace(label)
	if !validApp(app) || !idPattern.MatchString(key) || actor == "" || label == "" || len(label) > 240 || max < 1 || max > 10000 || !expires.After(now) || expires.After(now.Add(30*24*time.Hour)) {
		return Link{}, ErrInvalid
	}
	input, _ := json.Marshal(struct {
		Pool, Label string
		Max         int
		Expires     time.Time
	}{pool, label, max, expires})
	hash := sha256.Sum256(input)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, pool)
	if err != nil {
		return Link{}, err
	}
	var id string
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM offer_delivery_links WHERE app_id=? AND request_key=?`, app, key).Scan(&id, &previous)
	if err == nil {
		if string(previous) != string(hash[:]) {
			return Link{}, ErrConflict
		}
		tx.Rollback()
		return s.GetLink(ctx, app, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Link{}, err
	}
	if !p.Active || !p.ExpiresAt.After(now) || expires.After(p.ExpiresAt) {
		return Link{}, ErrUnavailable
	}
	id = uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_links(app_id,id,pool_id,request_key,request_hash,label,max_applications,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, app, id, pool, key, hash[:], label, max, now.UTC(), expires); err != nil {
		return Link{}, err
	}
	if err = event(ctx, tx, app, pool, actor, "link.create", 1, now); err != nil {
		return Link{}, err
	}
	if err = tx.Commit(); err != nil {
		return Link{}, err
	}
	return s.GetLink(ctx, app, id)
}

const linkColumns = `l.id,l.pool_id,l.label,l.max_applications,l.created_at,l.expires_at,l.revoked_at,(SELECT COUNT(*) FROM offer_delivery_requests r WHERE r.app_id=l.app_id AND r.link_id=l.id)`

func scanLink(row scanner) (Link, error) {
	var l Link
	err := row.Scan(&l.ID, &l.PoolID, &l.Label, &l.MaxApplications, &l.CreatedAt, &l.ExpiresAt, &l.RevokedAt, &l.Applications)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}
func (s Store) GetLink(ctx context.Context, app, id string) (Link, error) {
	return scanLink(s.DB.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM offer_delivery_links l WHERE app_id=? AND id=?`, app, id))
}
func (s Store) Links(ctx context.Context, app, pool string) ([]Link, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+linkColumns+` FROM offer_delivery_links l WHERE app_id=? AND pool_id=? ORDER BY created_at DESC LIMIT 100`, app, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s Store) RevokeLink(ctx context.Context, app, pool, id, actor string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.pool(ctx, tx, app, pool); err != nil {
		return err
	}
	l, err := scanLink(tx.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM offer_delivery_links l WHERE app_id=? AND id=? FOR UPDATE`, app, id))
	if err != nil {
		return err
	}
	if l.PoolID != pool {
		return ErrNotFound
	}
	if l.RevokedAt != nil {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_links SET revoked_at=? WHERE app_id=? AND id=?`, now.UTC(), app, id); err != nil {
		return err
	}
	if err = event(ctx, tx, app, pool, actor, "link.revoke", 1, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyLink counts applications under the pool lock shared with allocation and link revocation.
func (s Store) ApplyLink(ctx context.Context, app, linkID, key string, recipient Recipient, now time.Time) (Request, error) {
	if !validApp(app) || !recipient.Valid() {
		return Request{}, ErrInvalid
	}
	if _, err := uuid.Parse(key); err != nil {
		return Request{}, ErrInvalid
	}
	link, err := s.GetLink(ctx, app, linkID)
	if err != nil {
		return Request{}, err
	}
	// Channel is the operator's link label, not a client-controlled attribution.
	recipient.Channel = link.Label
	raw, err := json.Marshal(recipient)
	if err != nil {
		return Request{}, err
	}
	hash := sha256.Sum256(append([]byte(linkID+":"), raw...))
	requestKey := "claim:" + linkID + ":" + key
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback()
	p, err := s.pool(ctx, tx, app, link.PoolID)
	if err != nil {
		return Request{}, err
	}
	var existing string
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM offer_delivery_requests WHERE app_id=? AND request_key=?`, app, requestKey).Scan(&existing, &previous)
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
	link, err = scanLink(tx.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM offer_delivery_links l WHERE app_id=? AND id=? FOR UPDATE`, app, linkID))
	if err != nil {
		return Request{}, err
	}
	if link.RevokedAt != nil || !link.ExpiresAt.After(now) || link.Applications >= link.MaxApplications || !p.Active || !p.ExpiresAt.After(now) {
		return Request{}, ErrUnavailable
	}
	id := uuid.NewString()
	encrypted, nonce, err := s.Cipher.Encrypt(raw, aad(app, "recipient", id))
	if err != nil {
		return Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_requests(app_id,id,pool_id,request_key,request_hash,ciphertext,nonce,status,source,link_id,requested_at) VALUES(?,?,?,?,?,?,?,'requested','link',?,?)`, app, id, p.ID, requestKey, hash[:], encrypted, nonce, linkID, now.UTC()); err != nil {
		return Request{}, err
	}
	if err = event(ctx, tx, app, id, "applicant", "request.create", 1, now); err != nil {
		return Request{}, err
	}
	if err = tx.Commit(); err != nil {
		return Request{}, err
	}
	return s.Get(ctx, app, id)
}
