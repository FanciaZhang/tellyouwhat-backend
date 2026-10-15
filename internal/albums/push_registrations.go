package albums

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

var errPushRegistration = errors.New("album notification registration unavailable")
var errPushRegistrationConflict = errors.New("album notification registration changed")

type albumPushUpdate struct {
	RequestID        string `json:"requestID"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Enabled          bool   `json:"enabled"`
	DeviceToken      string `json:"deviceToken,omitempty"`
}

func (albumPushUpdate) String() string   { return "[redacted album push update]" }
func (albumPushUpdate) GoString() string { return "[redacted album push update]" }

type albumPushRegistration struct {
	ID       string `json:"registrationID,omitempty"`
	Revision int64  `json:"revision"`
	State    string `json:"state"`
}

type albumPushTarget struct {
	Registration albumPushRegistration
	Owner        string
	Token        string `json:"-"`
	RegisteredAt time.Time
}

func (albumPushTarget) String() string   { return "[redacted album push target]" }
func (albumPushTarget) GoString() string { return "[redacted album push target]" }

type albumPushRegistrations struct {
	db          *sql.DB
	cipher      AccountCipher
	environment string
}

func newAlbumPushRegistrations(db *sql.DB, cipher AccountCipher, environment string) (*albumPushRegistrations, error) {
	if db == nil || cipher == nil || (environment != "development" && environment != "production") {
		return nil, errPushRegistration
	}
	return &albumPushRegistrations{db, cipher, environment}, nil
}

func pushTokenDigest(token string) ([]byte, error) {
	if len(token) == 0 || len(token) > 1024 || token != strings.ToLower(token) {
		return nil, errPushRegistration
	}
	value, err := hex.DecodeString(token)
	if err != nil {
		return nil, errPushRegistration
	}
	digest := sha256.Sum256(value)
	return digest[:], nil
}

func pushTokenAAD(id, session, owner, environment string, revision int64) []byte {
	return []byte(fmt.Sprintf("albums/push/v1\x00%s\x00%s\x00%s\x00%s\x00%d", id, session, owner, environment, revision))
}

// Account -> grant -> session, matching sign-in. Session expiry blocks changes,
// while delivery eligibility deliberately follows revocation instead of expiry.
func lockPushSession(ctx context.Context, tx *sql.Tx, credential string, now time.Time) (id, owner string, created time.Time, err error) {
	digest, err := sessionDigest(credential)
	if err != nil || now.IsZero() {
		return "", "", time.Time{}, ErrSession
	}
	var grant, state string
	if err = tx.QueryRowContext(ctx, `SELECT session_id,account_id,grant_id FROM album_sessions WHERE token_sha256=?`, digest[:]).Scan(&id, &owner, &grant); err != nil {
		return "", "", time.Time{}, ErrSession
	}
	if err = tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=? FOR UPDATE`, owner).Scan(&state); err != nil || state != "active" {
		return "", "", time.Time{}, ErrSession
	}
	var revoked sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT revoked_at FROM album_apple_grants WHERE account_id=? AND grant_id=? FOR UPDATE`, owner, grant).Scan(&revoked); err != nil || revoked.Valid {
		return "", "", time.Time{}, ErrSession
	}
	var expires time.Time
	if err = tx.QueryRowContext(ctx, `SELECT expires_at,revoked_at,created_at FROM album_sessions WHERE session_id=? AND token_sha256=? FOR UPDATE`, id, digest[:]).Scan(&expires, &revoked, &created); err != nil || revoked.Valid || !now.Before(expires) {
		return "", "", time.Time{}, ErrSession
	}
	return id, owner, created, nil
}

func (r *albumPushRegistrations) current(ctx context.Context, credential string, now time.Time) (albumPushRegistration, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return albumPushRegistration{}, errPushRegistration
	}
	defer tx.Rollback()
	session, _, _, err := lockPushSession(ctx, tx, credential, now)
	if err != nil {
		return albumPushRegistration{}, err
	}
	current := albumPushRegistration{State: "disabled"}
	err = tx.QueryRowContext(ctx, `SELECT registration_id,revision,state FROM album_push_registrations WHERE session_id=? AND environment=? FOR UPDATE`, session, r.environment).Scan(&current.ID, &current.Revision, &current.State)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return albumPushRegistration{}, errPushRegistration
	}
	if err := tx.Commit(); err != nil {
		return albumPushRegistration{}, errPushRegistration
	}
	return current, nil
}

func (r *albumPushRegistrations) update(ctx context.Context, credential string, u albumPushUpdate, now time.Time) (albumPushRegistration, error) {
	if !validOwner(u.RequestID) || u.ExpectedRevision < 0 || u.ExpectedRevision >= math.MaxInt64 || now.IsZero() || (!u.Enabled && u.DeviceToken != "") {
		return albumPushRegistration{}, errPushRegistration
	}
	var digest []byte
	if u.Enabled {
		var err error
		digest, err = pushTokenDigest(u.DeviceToken)
		if err != nil {
			return albumPushRegistration{}, err
		}
	}
	payload, _ := json.Marshal(u)
	requestHash := sha256.Sum256(payload)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return albumPushRegistration{}, errPushRegistration
	}
	defer tx.Rollback()
	session, owner, created, err := lockPushSession(ctx, tx, credential, now)
	if err != nil {
		return albumPushRegistration{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO album_push_registrations(registration_id,session_id,environment) VALUES(?,?,?) ON DUPLICATE KEY UPDATE registration_id=registration_id`, uuid.NewString(), session, r.environment); err != nil {
		return albumPushRegistration{}, errPushRegistration
	}
	var current albumPushRegistration
	var request sql.NullString
	var hash []byte
	err = tx.QueryRowContext(ctx, `SELECT registration_id,revision,state,request_id,request_sha256 FROM album_push_registrations WHERE session_id=? AND environment=? FOR UPDATE`, session, r.environment).Scan(&current.ID, &current.Revision, &current.State, &request, &hash)
	if err != nil || current.Revision == math.MaxInt64 {
		return current, errPushRegistration
	}
	if current.State == "superseded" {
		return current, errPushRegistrationConflict
	}
	if request.Valid && request.String == u.RequestID {
		if !bytes.Equal(hash, requestHash[:]) {
			return current, errPushRegistrationConflict
		}
		if err := tx.Commit(); err != nil {
			return current, errPushRegistration
		}
		return current, nil
	}
	if current.Revision != u.ExpectedRevision {
		return current, errPushRegistrationConflict
	}
	if u.Enabled {
		var other, otherSession string
		var otherRevision int64
		err := tx.QueryRowContext(ctx, `SELECT registration_id,session_id,revision FROM album_push_registrations WHERE environment=? AND token_sha256=? FOR UPDATE`, r.environment, digest).Scan(&other, &otherSession, &otherRevision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return current, errPushRegistration
		}
		if err == nil && other != current.ID {
			var otherCreated time.Time
			if err := tx.QueryRowContext(ctx, `SELECT created_at FROM album_sessions WHERE session_id=?`, otherSession).Scan(&otherCreated); err != nil {
				return current, errPushRegistration
			}
			if !created.After(otherCreated) || otherRevision == math.MaxInt64 {
				return current, errPushRegistrationConflict
			}
			// Keep a tombstone: late work on the replaced session cannot restore its token.
			if _, err := tx.ExecContext(ctx, `UPDATE album_push_registrations SET state='superseded',revision=revision+1,token_sha256=NULL,encrypted_token=NULL,token_nonce=NULL,request_id=NULL,request_sha256=NULL WHERE registration_id=?`, other); err != nil {
				return current, errPushRegistration
			}
		}
	}
	current.Revision++
	current.State = "disabled"
	var encrypted, nonce []byte
	var registered any
	if u.Enabled {
		current.State = "enabled"
		encrypted, nonce, err = r.cipher.Encrypt([]byte(u.DeviceToken), pushTokenAAD(current.ID, session, owner, r.environment, current.Revision))
		if err != nil || len(nonce) != 12 || len(encrypted) <= len(u.DeviceToken) {
			return current, errPushRegistration
		}
		registered = now.UTC().Truncate(time.Microsecond)
	}
	// Disabling keeps only the token digest so an older session cannot reclaim it.
	if _, err := tx.ExecContext(ctx, `UPDATE album_push_registrations SET state=?,revision=?,token_sha256=COALESCE(?,token_sha256),encrypted_token=?,token_nonce=?,registered_at=?,request_id=?,request_sha256=? WHERE registration_id=?`, current.State, current.Revision, digest, encrypted, nonce, registered, u.RequestID, requestHash[:], current.ID); err != nil {
		return current, errPushRegistration
	}
	if err := tx.Commit(); err != nil {
		return current, errPushRegistration
	}
	return current, nil
}

// Internal exact-target read for the dispatcher. Never return decrypted tokens to an API.
func (r *albumPushRegistrations) target(ctx context.Context, owner, id string, revision int64) (albumPushTarget, error) {
	if !validOwner(owner) || !validOwner(id) || revision < 1 {
		return albumPushTarget{}, errPushRegistration
	}
	var target albumPushTarget
	var session string
	var digest, encrypted, nonce []byte
	err := r.db.QueryRowContext(ctx, `SELECT p.registration_id,p.revision,p.state,p.session_id,p.registered_at,p.token_sha256,p.encrypted_token,p.token_nonce FROM album_push_registrations p JOIN album_sessions s ON s.session_id=p.session_id JOIN album_accounts a ON a.account_id=s.account_id JOIN album_apple_grants g ON g.grant_id=s.grant_id AND g.account_id=a.account_id WHERE p.registration_id=? AND p.revision=? AND p.environment=? AND a.account_id=? AND p.state='enabled' AND s.revoked_at IS NULL AND a.state='active' AND g.revoked_at IS NULL`, id, revision, r.environment, owner).
		Scan(&target.Registration.ID, &target.Registration.Revision, &target.Registration.State, &session, &target.RegisteredAt, &digest, &encrypted, &nonce)
	if err != nil || len(nonce) != 12 {
		return albumPushTarget{}, errPushRegistration
	}
	plain, err := r.cipher.Decrypt(encrypted, nonce, pushTokenAAD(id, session, owner, r.environment, revision))
	if err != nil {
		return albumPushTarget{}, errPushRegistration
	}
	actual, err := pushTokenDigest(string(plain))
	if err != nil || !bytes.Equal(actual, digest) {
		return albumPushTarget{}, errPushRegistration
	}
	target.Owner, target.Token = owner, string(plain)
	return target, nil
}

// A late APNs 410 must not disable a newer registration of the same token.
func (r *albumPushRegistrations) invalidate(ctx context.Context, id string, revision int64, invalidatedAt time.Time) error {
	if !validOwner(id) || revision < 1 || revision == math.MaxInt64 || invalidatedAt.IsZero() {
		return errPushRegistration
	}
	_, err := r.db.ExecContext(ctx, `UPDATE album_push_registrations SET state='disabled',revision=revision+1,encrypted_token=NULL,token_nonce=NULL,request_id=NULL,request_sha256=NULL WHERE registration_id=? AND environment=? AND revision=? AND state='enabled' AND registered_at<=?`, id, r.environment, revision, invalidatedAt.UTC())
	if err != nil {
		return errPushRegistration
	}
	return nil
}
