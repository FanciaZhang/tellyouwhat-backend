// Package offerdelivery tracks named offer requests and irreversible code allocation.
package offerdelivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrInvalid     = errors.New("invalid offer delivery input")
	ErrConflict    = errors.New("offer delivery state changed")
	ErrUnavailable = errors.New("no confirmed available code")
	ErrNotFound    = errors.New("offer delivery record not found")
)
var codePattern = regexp.MustCompile(`^[A-Z0-9]{4,64}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type Cipher interface {
	Encrypt([]byte, []byte) ([]byte, []byte, error)
	Decrypt([]byte, []byte, []byte) ([]byte, error)
}
type Store struct {
	DB     *sql.DB
	Cipher Cipher
}
type Pool struct {
	SubscriptionID string    `json:"subscriptionID"`
	ProductID      string    `json:"productID"`
	Code           string    `json:"-"`
	ID             string    `json:"id"`
	OfferID        string    `json:"offerID"`
	OfferName      string    `json:"offerName"`
	Kind           string    `json:"kind"`
	Environment    string    `json:"environment"`
	Capacity       int       `json:"capacity"`
	Active         bool      `json:"active"`
	ExpiresAt      time.Time `json:"expiresAt"`
	SyncedAt       time.Time `json:"syncedAt"`
}
type Recipient struct {
	Name      string `json:"name"`
	Contact   string `json:"contact"`
	Reference string `json:"reference"`
	Channel   string `json:"channel"`
	Note      string `json:"note"`
}

func (r Recipient) Valid() bool {
	return strings.TrimSpace(r.Name) != "" && utf8.ValidString(r.Name+r.Contact+r.Reference+r.Channel+r.Note) && len(r.Name) <= 240 && len(r.Contact) <= 500 && len(r.Reference) <= 240 && len(r.Channel) <= 240 && len(r.Note) <= 3000
}

type Request struct {
	ClaimedAt          *time.Time `json:"claimedAt,omitempty"`
	ClaimGeneration    int        `json:"claimGeneration"`
	ClaimExpiresAt     *time.Time `json:"claimExpiresAt,omitempty"`
	Source             string     `json:"source"`
	ID                 string     `json:"id"`
	PoolID             string     `json:"poolID"`
	Recipient          Recipient  `json:"recipient"`
	Status             string     `json:"status"`
	Version            int        `json:"version"`
	CodeID             string     `json:"codeID,omitempty"`
	RequestedAt        time.Time  `json:"requestedAt"`
	AssignedAt         *time.Time `json:"assignedAt,omitempty"`
	DeliveredAt        *time.Time `json:"deliveredAt,omitempty"`
	ReportedRedeemedAt *time.Time `json:"reportedRedeemedAt,omitempty"`
	VerifiedAt         *time.Time `json:"verifiedAt,omitempty"`
}
type Summary struct {
	Claimed            int `json:"claimed"`
	Applications       int `json:"applications"`
	ExternalDeliveries int `json:"externalDeliveries"`
	Imported           int `json:"imported"`
	Available          int `json:"available"`
	External           int `json:"external"`
	AssignedCodes      int `json:"assignedCodes"`
	AssignedRequests   int `json:"assignedRequests"`
	Requests           int `json:"requests"`
	Pending            int `json:"pending"`
	Delivered          int `json:"delivered"`
	ReportedRedeemed   int `json:"reportedRedeemed"`
	LinkedVerified     int `json:"linkedVerified"`
}

func aad(app, kind, id string) []byte { return []byte("offer-delivery:" + app + ":" + kind + ":" + id) }
func validApp(app string) bool        { return app == "health" || app == "journal" }

// SyncPool accepts only metadata freshly read through the App-scoped Apple client.
func (s Store) SyncPool(ctx context.Context, app string, p Pool) error {
	if !validApp(app) || !idPattern.MatchString(p.ID) || !idPattern.MatchString(p.OfferID) || p.OfferName == "" || len(p.OfferName) > 1000 || (p.Kind != "oneTime" && p.Kind != "custom") || (p.Environment != "production" && p.Environment != "sandbox") || p.Capacity < 1 || p.Capacity > 1000000 || p.ExpiresAt.IsZero() || p.SyncedAt.IsZero() {
		return ErrInvalid
	}
	var encrypted, nonce []byte
	if p.Kind == "custom" {
		if !codePattern.MatchString(p.Code) {
			return ErrInvalid
		}
		var err error
		encrypted, nonce, err = s.Cipher.Encrypt([]byte(p.Code), aad(app, "pool", p.ID))
		if err != nil {
			return err
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := s.pool(ctx, tx, app, p.ID)
	if err == nil && (previous.OfferID != p.OfferID || previous.ProductID != p.ProductID || previous.SubscriptionID != p.SubscriptionID || previous.Kind != p.Kind || previous.Environment != p.Environment) {
		return ErrConflict
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_pools(app_id,pool_id,offer_id,offer_name,subscription_id,product_id,kind,environment,capacity,active,expires_at,synced_at,ciphertext,nonce) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE offer_name=VALUES(offer_name),capacity=VALUES(capacity),active=VALUES(active),expires_at=VALUES(expires_at),synced_at=VALUES(synced_at),ciphertext=VALUES(ciphertext),nonce=VALUES(nonce)`, app, p.ID, p.OfferID, p.OfferName, p.SubscriptionID, p.ProductID, p.Kind, p.Environment, p.Capacity, p.Active, p.ExpiresAt.UTC(), p.SyncedAt.UTC(), encrypted, nonce)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) pool(ctx context.Context, tx *sql.Tx, app, id string) (Pool, error) {
	var p Pool
	p.ID = id
	err := tx.QueryRowContext(ctx, `SELECT offer_id,offer_name,subscription_id,product_id,kind,environment,capacity,active,expires_at,synced_at FROM offer_delivery_pools WHERE app_id=? AND pool_id=? FOR UPDATE`, app, id).Scan(&p.OfferID, &p.OfferName, &p.SubscriptionID, &p.ProductID, &p.Kind, &p.Environment, &p.Capacity, &p.Active, &p.ExpiresAt, &p.SyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}
func event(ctx context.Context, tx *sql.Tx, app, target, actor, action string, n int, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO offer_delivery_events(app_id,target_id,actor_id,action,quantity,created_at) VALUES(?,?,?,?,?,?)`, app, target, actor, action, n, now.UTC())
	return err
}

// ParseCSV validates the complete Apple batch before any inventory is persisted.
func ParseCSV(raw []byte, expected int) ([]string, error) {
	if len(raw) > 8<<20 || expected < 1 || expected > 25000 {
		return nil, ErrInvalid
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\ufeff")))
	r.FieldsPerRecord = -1
	var codes []string
	seen := map[string]bool{}
	column := 0
	first := true
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(row) == 0 {
			return nil, ErrInvalid
		}
		if first {
			first = false
			header := false
			for i, v := range row {
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "code", "offer code", "offer codes", "codes":
					column = i
					header = true
				}
			}
			if header {
				continue
			}
		}
		if column >= len(row) {
			return nil, ErrInvalid
		}
		v := strings.ToUpper(strings.TrimSpace(row[column]))
		if !codePattern.MatchString(v) || seen[v] {
			return nil, ErrInvalid
		}
		seen[v] = true
		codes = append(codes, v)
		if len(codes) > expected {
			return nil, ErrInvalid
		}
	}
	if len(codes) != expected {
		return nil, ErrInvalid
	}
	return codes, nil
}

// ImportCodes never assumes codes exported outside this system are still unissued.
func (s Store) ImportCodes(ctx context.Context, app, pool, actor string, codes []string, now time.Time) error {
	if !validApp(app) || actor == "" || len(codes) == 0 || len(codes) > 25000 {
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
	if p.Kind != "oneTime" || len(codes) != p.Capacity {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if !codePattern.MatchString(code) || seen[code] {
			return ErrInvalid
		}
		seen[code] = true
		hash := sha256.Sum256([]byte(code))
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT pool_id FROM offer_delivery_codes WHERE app_id=? AND code_hash=?`, app, hash[:]).Scan(&existing)
		if err == nil {
			if existing != pool {
				return ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id := uuid.NewString()
		encrypted, nonce, err := s.Cipher.Encrypt([]byte(code), aad(app, "code", id))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_codes(app_id,id,pool_id,code_hash,ciphertext,nonce,inventory_state,created_at) VALUES(?,?,?,?,?,?,'external',?)`, app, id, pool, hash[:], encrypted, nonce, now.UTC()); err != nil {
			return err
		}
	}
	if err = event(ctx, tx, app, pool, actor, "inventory.import", len(codes), now); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmAvailable is an explicit operator inventory declaration, not an Apple redemption result.
func (s Store) ConfirmAvailable(ctx context.Context, app, pool, actor string, codes []string, now time.Time) error {
	if !validApp(app) || actor == "" || len(codes) == 0 || len(codes) > 25000 {
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
	if !p.Active || !p.ExpiresAt.After(now) {
		return ErrUnavailable
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if !codePattern.MatchString(code) || seen[code] {
			return ErrInvalid
		}
		seen[code] = true
		hash := sha256.Sum256([]byte(code))
		var id, status string
		err = tx.QueryRowContext(ctx, `SELECT id,inventory_state FROM offer_delivery_codes WHERE app_id=? AND pool_id=? AND code_hash=? FOR UPDATE`, app, pool, hash[:]).Scan(&id, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == "assigned" {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='available' WHERE app_id=? AND id=?`, app, id); err != nil {
			return err
		}
	}
	if err = event(ctx, tx, app, pool, actor, "inventory.confirm_available", len(codes), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) CreateRequest(ctx context.Context, app, pool, key, actor string, r Recipient, now time.Time) (Request, error) {
	return s.createRequest(ctx, app, pool, key, actor, r, "manual", now)
}
func (s Store) createRequest(ctx context.Context, app, pool, key, actor string, r Recipient, source string, now time.Time) (Request, error) {
	if !validApp(app) || !idPattern.MatchString(key) || !r.Valid() || actor == "" {
		return Request{}, ErrInvalid
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Request{}, err
	}
	hash := sha256.Sum256(append([]byte(source+":"+pool+":"), raw...))
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
	if !p.Active || !p.ExpiresAt.After(now) {
		return Request{}, ErrUnavailable
	}
	id := uuid.NewString()
	encrypted, nonce, err := s.Cipher.Encrypt(raw, aad(app, "recipient", id))
	if err != nil {
		return Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_requests(app_id,id,pool_id,request_key,request_hash,ciphertext,nonce,source,status,requested_at) VALUES(?,?,?,?,?,?,?,?,'requested',?)`, app, id, pool, key, hash[:], encrypted, nonce, source, now.UTC()); err != nil {
		return Request{}, err
	}
	if err = event(ctx, tx, app, id, actor, "request.create", 1, now); err != nil {
		return Request{}, err
	}
	if err = tx.Commit(); err != nil {
		return Request{}, err
	}
	return s.Get(ctx, app, id)
}

const requestColumns = `id,pool_id,ciphertext,nonce,status,source,version,COALESCE(code_id,''),requested_at,assigned_at,delivered_at,claimed_at,reported_redeemed_at,(SELECT l.linked_at FROM offer_delivery_verified_links l WHERE l.app_id=offer_delivery_requests.app_id AND l.request_id=offer_delivery_requests.id),claim_generation,claim_expires_at`

type scanner interface{ Scan(...any) error }

func (s Store) scan(app string, row scanner) (Request, error) {
	var r Request
	var raw, nonce []byte
	err := row.Scan(&r.ID, &r.PoolID, &raw, &nonce, &r.Status, &r.Source, &r.Version, &r.CodeID, &r.RequestedAt, &r.AssignedAt, &r.DeliveredAt, &r.ClaimedAt, &r.ReportedRedeemedAt, &r.VerifiedAt, &r.ClaimGeneration, &r.ClaimExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	raw, err = s.Cipher.Decrypt(raw, nonce, aad(app, "recipient", r.ID))
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(raw, &r.Recipient)
	return r, err
}
func (s Store) Get(ctx context.Context, app, id string) (Request, error) {
	return s.scan(app, s.DB.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND id=?`, app, id))
}
func (s Store) List(ctx context.Context, app, pool, cursor string) ([]Request, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND pool_id=? AND id>? ORDER BY id LIMIT 101`, app, pool, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		r, err := s.scan(app, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s Store) Summary(ctx context.Context, app, pool string) (Summary, error) {
	var r Summary
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(inventory_state='available'),0),COALESCE(SUM(inventory_state='external'),0),COALESCE(SUM(inventory_state='assigned'),0) FROM offer_delivery_codes WHERE app_id=? AND pool_id=?`, app, pool).Scan(&r.Imported, &r.Available, &r.External, &r.AssignedCodes)
	if err != nil {
		return r, err
	}
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(source='manual'),0),COALESCE(SUM(source='external'),0),COALESCE(SUM(status='requested'),0),COALESCE(SUM(assigned_at IS NOT NULL),0),COALESCE(SUM(delivered_at IS NOT NULL),0),COALESCE(SUM(claimed_at IS NOT NULL),0),COALESCE(SUM(reported_redeemed_at IS NOT NULL),0) FROM offer_delivery_requests WHERE app_id=? AND pool_id=?`, app, pool).Scan(&r.Requests, &r.Applications, &r.ExternalDeliveries, &r.Pending, &r.AssignedRequests, &r.Delivered, &r.Claimed, &r.ReportedRedeemed)
	if err != nil {
		return r, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM offer_delivery_verified_links l JOIN offer_delivery_requests r ON r.app_id=l.app_id AND r.id=l.request_id WHERE r.app_id=? AND r.pool_id=?`, app, pool).Scan(&r.LinkedVerified); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// Assign reserves one code forever, including if the request is cancelled later.
func (s Store) Assign(ctx context.Context, app, id, actor string, version int, now time.Time) (Request, error) {
	if !validApp(app) || actor == "" {
		return Request{}, ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback()
	var pool string
	if err = tx.QueryRowContext(ctx, `SELECT pool_id FROM offer_delivery_requests WHERE app_id=? AND id=?`, app, id).Scan(&pool); errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	if err != nil {
		return Request{}, err
	}
	p, err := s.pool(ctx, tx, app, pool)
	if err != nil {
		return Request{}, err
	}
	r, err := s.scan(app, tx.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM offer_delivery_requests WHERE app_id=? AND id=? FOR UPDATE`, app, id))
	if err != nil {
		return r, err
	}
	if r.Version != version || r.Status != "requested" {
		return r, ErrConflict
	}
	if !p.Active || !p.ExpiresAt.After(now) {
		return r, ErrUnavailable
	}
	var codeID string
	if p.Kind == "oneTime" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM offer_delivery_codes WHERE app_id=? AND pool_id=? AND inventory_state='available' ORDER BY id LIMIT 1 FOR UPDATE`, app, pool).Scan(&codeID)
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrUnavailable
		}
		if err != nil {
			return r, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='assigned' WHERE app_id=? AND id=?`, app, codeID); err != nil {
			return r, err
		}
	} else {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM offer_delivery_requests WHERE app_id=? AND pool_id=? AND assigned_at IS NOT NULL`, app, pool).Scan(&count); err != nil {
			return r, err
		}
		if count >= p.Capacity {
			return r, ErrUnavailable
		}
	}
	var codeValue any
	if codeID != "" {
		codeValue = codeID
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET code_id=?,status='assigned',version=version+1,assigned_at=? WHERE app_id=? AND id=?`, codeValue, now.UTC(), app, id); err != nil {
		return r, err
	}
	if err = event(ctx, tx, app, id, actor, "request.assign", 1, now); err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return s.Get(ctx, app, id)
}

// Reveal records exposure before returning the secret code. Repeated reads are auditable.
func (s Store) Reveal(ctx context.Context, app, id, actor string, now time.Time) (string, error) {
	if !validApp(app) || actor == "" {
		return "", ErrInvalid
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var codeID, status string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(code_id,''),status FROM offer_delivery_requests WHERE app_id=? AND id=? FOR UPDATE`, app, id).Scan(&codeID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if status != "assigned" && status != "delivered" {
		return "", ErrConflict
	}
	var raw, nonce []byte
	contextID := codeID
	kind := "code"
	if codeID != "" {
		err = tx.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM offer_delivery_codes WHERE app_id=? AND id=?`, app, codeID).Scan(&raw, &nonce)
	} else {
		kind = "pool"
		err = tx.QueryRowContext(ctx, `SELECT p.pool_id,p.ciphertext,p.nonce FROM offer_delivery_pools p JOIN offer_delivery_requests r ON r.app_id=p.app_id AND r.pool_id=p.pool_id WHERE r.app_id=? AND r.id=? AND p.kind='custom'`, app, id).Scan(&contextID, &raw, &nonce)
	}
	if err != nil {
		return "", err
	}
	raw, err = s.Cipher.Decrypt(raw, nonce, aad(app, kind, contextID))
	if err != nil {
		return "", err
	}
	if err = event(ctx, tx, app, id, actor, "code.reveal", 1, now); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s Store) Transition(ctx context.Context, app, id, actor, action string, version int, now time.Time) (Request, error) {
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
	if version != r.Version {
		return r, ErrConflict
	}
	var update string
	switch action {
	case "deliver":
		if r.Status != "assigned" {
			return r, ErrConflict
		}
		update = "status='delivered',delivered_at=?"
	case "report_redeemed":
		if r.Status != "delivered" || r.ReportedRedeemedAt != nil {
			return r, ErrConflict
		}
		update = "reported_redeemed_at=?"
	case "cancel":
		if r.Status != "requested" && r.Status != "assigned" {
			return r, ErrConflict
		}
		update = "status='cancelled'"
	case "reject":
		if r.Status != "requested" {
			return r, ErrConflict
		}
		update = "status='rejected'"
	default:
		return r, ErrInvalid
	}
	args := []any{app, id}
	if action == "deliver" || action == "report_redeemed" {
		args = append([]any{now.UTC()}, args...)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET `+update+`,version=version+1 WHERE app_id=? AND id=?`, args...); err != nil {
		return r, err
	}
	if err = event(ctx, tx, app, id, actor, "request."+action, 1, now); err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return s.Get(ctx, app, id)
}

// LinkVerified records an administrator's association to an Apple-verified subscription.
// Apple does not identify the redeemed one-time code or the named recipient.
func (s Store) LinkVerified(ctx context.Context, app, id, actor, originalHex string, version int, now time.Time) (Request, error) {
	original, err := hex.DecodeString(originalHex)
	if err != nil || len(original) != 32 || !validApp(app) || actor == "" {
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
	if r.Version != version || r.Status != "delivered" || r.VerifiedAt != nil {
		return r, ErrConflict
	}
	var offerName, environment, productID string
	if err = tx.QueryRowContext(ctx, `SELECT offer_name,environment,product_id FROM offer_delivery_pools WHERE app_id=? AND pool_id=?`, app, r.PoolID).Scan(&offerName, &environment, &productID); err != nil {
		return r, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM app_store_offer_redemptions WHERE app_id=? AND environment=? AND BINARY offer_identifier=BINARY ? AND product_id=? AND offer_type=3 AND original_transaction_hash=? LIMIT 1 FOR UPDATE`, app, environment, offerName, productID, original).Scan(&count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	if count == 0 {
		return r, ErrNotFound
	}
	// A single observed subscription cannot be counted against several named recipients.
	if _, err = tx.ExecContext(ctx, `INSERT INTO offer_delivery_verified_links(app_id,request_id,environment,offer_name,original_hash,linked_at) VALUES(?,?,?,?,?,?)`, app, id, environment, offerName, original, now.UTC()); err != nil {
		return r, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE offer_delivery_requests SET version=version+1 WHERE app_id=? AND id=?`, app, id); err != nil {
		return r, err
	}
	if err = event(ctx, tx, app, id, actor, "request.link_verified", 1, now); err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	return s.Get(ctx, app, id)
}

// RecordExport removes all unassigned codes from managed availability before exposing a CSV.
func (s Store) RecordExport(ctx context.Context, app, pool, actor string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.pool(ctx, tx, app, pool); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE offer_delivery_codes SET inventory_state='external' WHERE app_id=? AND pool_id=? AND inventory_state='available'`, app, pool)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err = event(ctx, tx, app, pool, actor, "inventory.export", int(n), now); err != nil {
		return err
	}
	return tx.Commit()
}
