package offerdelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

var ErrPersonalUncertain = errors.New("personal code creation requires Apple reconciliation")

type PersonalCloud interface {
	ListOffers(context.Context) ([]appstoreconnect.Offer, error)
	ListCodePools(context.Context, string) ([]appstoreconnect.CodePool, error)
	CreateCustomCode(context.Context, string, string, int, string) (appstoreconnect.CodePool, error)
}

type PersonalDelivery struct {
	ID         string    `json:"id"`
	OfferID    string    `json:"offerID"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	PoolID     string    `json:"poolID,omitempty"`
	RequestID  string    `json:"requestID,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Code       string    `json:"-"`
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
	var secret struct{ Name, Code, Expiration string }
	if err = json.Unmarshal(raw, &secret); err != nil {
		return v, err
	}
	v.Name, v.Code, v.Expiration = secret.Name, secret.Code, secret.Expiration
	return v, nil
}

// PreparePersonal persists the recipient and random code before any cloud write.
func (s Store) PreparePersonal(ctx context.Context, app, offer, id, name, expiry string, now time.Time) (PersonalDelivery, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PersonalDelivery{}, ErrInvalid
	}
	if !validApp(app) || !idPattern.MatchString(offer) || !(Recipient{Name: name}).Valid() {
		return PersonalDelivery{}, ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", expiry); err != nil {
		return PersonalDelivery{}, ErrInvalid
	}
	input, _ := json.Marshal([]string{offer, name, expiry})
	hash := sha256.Sum256(input)
	return s.preparePersonal(ctx, app, offer, id, name, expiry, hash, now)
}
func (s Store) preparePersonal(ctx context.Context, app, offer, id, name, expiry string, hash [32]byte, now time.Time) (PersonalDelivery, error) {
	random := uuid.New()
	code := "TYW" + strings.ToUpper(hex.EncodeToString(random[:]))
	raw, _ := json.Marshal(struct{ Name, Code, Expiration string }{name, code, expiry})
	encrypted, nonce, err := s.Cipher.Encrypt(raw, aad(app, "personal", id))
	if err != nil {
		return PersonalDelivery{}, err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO offer_personal_deliveries(app_id,id,offer_id,input_hash,ciphertext,nonce,state,created_at) VALUES(?,?,?,?,?,?,'prepared',?) ON DUPLICATE KEY UPDATE id=id`, app, id, offer, hash[:], encrypted, nonce, now.UTC())
	if err != nil {
		return PersonalDelivery{}, err
	}
	var oldHash []byte
	if err = s.DB.QueryRowContext(ctx, `SELECT input_hash FROM offer_personal_deliveries WHERE app_id=? AND id=?`, app, id).Scan(&oldHash); err != nil {
		return PersonalDelivery{}, err
	}
	if !bytes.Equal(hash[:], oldHash) {
		return PersonalDelivery{}, ErrConflict
	}
	return s.Personal(ctx, app, id)
}

// CompletePersonal is resumable after any local failure. Once a cloud create
// starts, retry only reconciles its random code; it never creates a second code.
func (s Store) CompletePersonal(ctx context.Context, app, id, actor string, cloud PersonalCloud, now time.Time) (PersonalDelivery, Request, error) {
	var request Request
	connection, err := s.DB.Conn(ctx)
	if err != nil {
		return PersonalDelivery{}, request, err
	}
	defer connection.Close()
	lockHash := sha256.Sum256([]byte(app + ":" + id))
	lockName := "offer-personal:" + hex.EncodeToString(lockHash[:20])
	var locked int
	if err = connection.QueryRowContext(ctx, `SELECT GET_LOCK(?,0)`, lockName).Scan(&locked); err != nil || locked != 1 {
		return PersonalDelivery{}, request, ErrConflict
	}
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		connection.ExecContext(release, `SELECT RELEASE_LOCK(?)`, lockName)
	}()
	v, err := s.Personal(ctx, app, id)
	if err != nil {
		return v, request, err
	}
	if v.State == "rejected" {
		return v, request, appstoreconnect.ErrRejected
	}
	if v.State == "ready" {
		request, err = s.Get(ctx, app, v.RequestID)
		return v, request, err
	}
	offers, err := cloud.ListOffers(ctx)
	if err != nil {
		return v, request, err
	}
	var offer appstoreconnect.Offer
	for _, o := range offers {
		if o.ID == v.OfferID {
			offer = o
			break
		}
	}
	if offer.ID == "" || !offer.Active || offer.SubscriptionID == "" || offer.ProductID == "" {
		return v, request, ErrUnavailable
	}
	var pool appstoreconnect.CodePool
	if v.State == "prepared" {
		// Durable intent precedes the network side effect, so a crash is reconcilable.
		if _, err = s.DB.ExecContext(ctx, `UPDATE offer_personal_deliveries SET state='creating' WHERE app_id=? AND id=? AND state='prepared'`, app, id); err != nil {
			return v, request, err
		}
		pool, err = cloud.CreateCustomCode(ctx, offer.ID, v.Code, 1, v.Expiration)
		if err != nil {
			if errors.Is(err, appstoreconnect.ErrForbidden) {
				_, updateErr := s.DB.ExecContext(ctx, `UPDATE offer_personal_deliveries SET state='prepared' WHERE app_id=? AND id=?`, app, id)
				if updateErr != nil {
					return v, request, updateErr
				}
				return v, request, err
			}
			if errors.Is(err, appstoreconnect.ErrRejected) {
				_, updateErr := s.DB.ExecContext(ctx, `UPDATE offer_personal_deliveries SET state='rejected' WHERE app_id=? AND id=?`, app, id)
				if updateErr != nil {
					return v, request, updateErr
				}
				return v, request, err
			}
			return v, request, ErrPersonalUncertain
		}
	} else {
		pools, err := cloud.ListCodePools(ctx, offer.ID)
		if err != nil {
			return v, request, err
		}
		for _, candidate := range pools {
			if candidate.Kind == "custom" && candidate.Code == v.Code {
				if pool.ID != "" {
					return v, request, ErrConflict
				}
				pool = candidate
			}
		}
		if pool.ID == "" {
			return v, request, ErrPersonalUncertain
		}
	}
	if pool.ID == "" || pool.Kind != "custom" || pool.Code != v.Code || pool.NumberOfCodes != 1 || !pool.Active || pool.ExpirationDate != v.Expiration {
		return v, request, ErrConflict
	}
	expiry, err := AppleCodeExpiry(v.Expiration)
	if err != nil {
		return v, request, err
	}
	p := Pool{ID: pool.ID, OfferID: offer.ID, OfferName: offer.Name, SubscriptionID: offer.SubscriptionID, ProductID: offer.ProductID, Kind: "custom", Code: v.Code, Environment: "production", Capacity: 1, Active: true, ExpiresAt: expiry, SyncedAt: now}
	if err = s.SyncPool(ctx, app, p); err != nil {
		return v, request, err
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE offer_personal_deliveries SET state='provisioned',pool_id=? WHERE app_id=? AND id=?`, pool.ID, app, id); err != nil {
		return v, request, err
	}
	request, err = s.createRequest(ctx, app, p.ID, "personal:"+id, actor, Recipient{Name: v.Name, Channel: "熟人专属码"}, "personal", now)
	if err != nil {
		return v, request, err
	}
	if request.Status == "requested" {
		request, err = s.Assign(ctx, app, request.ID, actor, request.Version, now)
		if err != nil {
			return v, request, err
		}
	}
	if request.ClaimGeneration == 0 {
		request, err = s.SetClaimLink(ctx, app, request.ID, actor, request.Version, true, now)
		if err != nil {
			return v, request, err
		}
	}
	if _, err = s.DB.ExecContext(ctx, `UPDATE offer_personal_deliveries SET state='ready',request_id=? WHERE app_id=? AND id=?`, request.ID, app, id); err != nil {
		return v, request, err
	}
	v, err = s.Personal(ctx, app, id)
	return v, request, err
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
