package albums

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrSession = errors.New("album session is invalid or expired")

// Session tokens are opaque random credentials, independent of Apple tokens.
// The database stores only their hashes. Clients must keep them in Keychain.
type AlbumSession struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	AccountID string    `json:"accountID"`
}

func (AlbumSession) String() string       { return "[redacted album session]" }
func (AlbumSession) GoString() string     { return "[redacted album session]" }
func (AlbumSession) LogValue() slog.Value { return slog.StringValue("[redacted album session]") }

type MySQLSessions struct {
	db  *sql.DB
	now func() time.Time
}

func NewMySQLSessions(db *sql.DB, now func() time.Time) (*MySQLSessions, error) {
	if db == nil {
		return nil, errors.New("album session database is required")
	}
	if now == nil {
		now = time.Now
	}
	return &MySQLSessions{db, now}, nil
}

// Issue is server-only: login must come from RegisterAppleLogin, not request JSON.
// Lock order matches account registration: account before grant before session.
func (s *MySQLSessions) Issue(ctx context.Context, login AccountLogin) (AlbumSession, error) {
	if !validOwner(login.AccountID) || !validOwner(login.GrantID) {
		return AlbumSession{}, ErrSession
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return AlbumSession{}, ErrSession
	}
	token := "als1_" + base64.RawURLEncoding.EncodeToString(secret[:])
	digest, _ := sessionDigest(token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AlbumSession{}, ErrSession
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM album_accounts WHERE account_id=? FOR UPDATE`, login.AccountID).Scan(&state); err != nil || state != "active" {
		return AlbumSession{}, ErrSession
	}
	var revoked sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM album_apple_grants WHERE account_id=? AND grant_id=? FOR UPDATE`, login.AccountID, login.GrantID).Scan(&revoked); err != nil || revoked.Valid {
		return AlbumSession{}, ErrSession
	}
	session := AlbumSession{Token: token, ExpiresAt: s.now().UTC().Add(30 * 24 * time.Hour), AccountID: login.AccountID}
	if _, err := tx.ExecContext(ctx, `INSERT INTO album_sessions(session_id,account_id,grant_id,token_sha256,expires_at) VALUES (?,?,?,?,?)`, uuid.NewString(), login.AccountID, login.GrantID, digest[:], session.ExpiresAt); err != nil {
		return AlbumSession{}, ErrSession
	}
	if err := tx.Commit(); err != nil {
		return AlbumSession{}, ErrSession
	}
	return session, nil
}

func sessionDigest(token string) ([32]byte, error) {
	if len(token) != 48 || !strings.HasPrefix(token, "als1_") {
		return [32]byte{}, ErrSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[5:])
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token[5:] {
		return [32]byte{}, ErrSession
	}
	return sha256.Sum256([]byte(token)), nil
}
func requestSessionToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", ErrSession
	}
	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrSession
	}
	if _, err := sessionDigest(parts[1]); err != nil {
		return "", ErrSession
	}
	return parts[1], nil
}

// Read current account/grant state on every request. No stale authorization
// cache may continue accepting a revoked session, grant or disabled account.
func (s *MySQLSessions) Authenticate(ctx context.Context, r *http.Request) (AccountIdentity, error) {
	token, err := requestSessionToken(r)
	if err != nil {
		return AccountIdentity{}, ErrSession
	}
	digest, _ := sessionDigest(token)
	var account string
	err = s.db.QueryRowContext(ctx, `SELECT s.account_id FROM album_sessions s JOIN album_accounts a ON a.account_id=s.account_id JOIN album_apple_grants g ON g.grant_id=s.grant_id AND g.account_id=s.account_id WHERE s.token_sha256=? AND s.expires_at>? AND s.revoked_at IS NULL AND a.state='active' AND g.revoked_at IS NULL`, digest[:], s.now().UTC()).Scan(&account)
	if err != nil || !validOwner(account) {
		return AccountIdentity{}, ErrSession
	}
	return AccountIdentity{AppID: "albums", AccountID: account}, nil
}

// Revoke is idempotent and scoped to the presented credential; no caller-
// supplied account ID is accepted. It also works for expired sessions.
func (s *MySQLSessions) Revoke(ctx context.Context, token string) error {
	digest, err := sessionDigest(token)
	if err != nil {
		return ErrSession
	}
	_, err = s.db.ExecContext(ctx, `UPDATE album_sessions SET revoked_at=COALESCE(revoked_at,?) WHERE token_sha256=?`, s.now().UTC(), digest[:])
	if err != nil {
		return ErrSession
	}
	return nil
}

var _ AccountAuthenticator = (*MySQLSessions)(nil)
