package albums

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

var ErrLoginChallenge = errors.New("album login challenge is invalid or already used")

// Nonce is already hashed: pass it directly to ASAuthorizationAppleIDRequest.nonce.
// Proof is a separate client-held secret; neither it nor the Apple authorization
// result may be logged. API integration must rate-limit challenge issuance.
type LoginChallenge struct {
	ID        string    `json:"id"`
	Nonce     string    `json:"nonce"`
	Proof     string    `json:"proof"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (LoginChallenge) String() string   { return "[redacted album login challenge]" }
func (LoginChallenge) GoString() string { return "[redacted album login challenge]" }
func (LoginChallenge) LogValue() slog.Value {
	return slog.StringValue("[redacted album login challenge]")
}

type StoredLoginChallenge struct {
	ID          string
	Nonce       string
	ProofSHA256 string
	ExpiresAt   time.Time
}

type LoginChallenges interface {
	Insert(context.Context, StoredLoginChallenge) error
	Consume(context.Context, string, string, time.Time) (string, error)
}

type LoginChallengeService struct {
	store LoginChallenges
	now   func() time.Time
}

func NewLoginChallengeService(store LoginChallenges, now func() time.Time) (*LoginChallengeService, error) {
	if store == nil {
		return nil, errors.New("login challenge store is required")
	}
	if now == nil {
		now = time.Now
	}
	return &LoginChallengeService{store, now}, nil
}
func (s *LoginChallengeService) Issue(ctx context.Context) (LoginChallenge, error) {
	var proof, nonce [32]byte
	if _, err := rand.Read(proof[:]); err != nil {
		return LoginChallenge{}, ErrLoginChallenge
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return LoginChallenge{}, ErrLoginChallenge
	}
	digest := sha256.Sum256(nonce[:])
	challenge := LoginChallenge{ID: uuid.NewString(), Nonce: hex.EncodeToString(digest[:]), Proof: base64.RawURLEncoding.EncodeToString(proof[:]), ExpiresAt: s.now().UTC().Add(5 * time.Minute)}
	hash := sha256.Sum256(proof[:])
	if err := s.store.Insert(ctx, StoredLoginChallenge{challenge.ID, challenge.Nonce, hex.EncodeToString(hash[:]), challenge.ExpiresAt}); err != nil {
		return LoginChallenge{}, ErrLoginChallenge
	}
	return challenge, nil
}
func (s *LoginChallengeService) Consume(ctx context.Context, id, proof string) (string, error) {
	if !validOwner(id) || len(proof) != 43 {
		return "", ErrLoginChallenge
	}
	raw, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != proof {
		return "", ErrLoginChallenge
	}
	digest := sha256.Sum256(raw)
	nonce, err := s.store.Consume(ctx, id, hex.EncodeToString(digest[:]), s.now().UTC())
	if err != nil || !loginNoncePattern.MatchString(nonce) {
		return "", ErrLoginChallenge
	}
	return nonce, nil
}

type MySQLLoginChallenges struct{ db *sql.DB }

func NewMySQLLoginChallenges(db *sql.DB) *MySQLLoginChallenges { return &MySQLLoginChallenges{db} }
func (r *MySQLLoginChallenges) Insert(ctx context.Context, c StoredLoginChallenge) error {
	if !validOwner(c.ID) || !loginNoncePattern.MatchString(c.Nonce) || !loginNoncePattern.MatchString(c.ProofSHA256) {
		return ErrLoginChallenge
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO album_login_challenges(challenge_id,proof_sha256,apple_nonce,expires_at) VALUES (?,?,?,?)`, c.ID, c.ProofSHA256, c.Nonce, c.ExpiresAt)
	return err
}
func (r *MySQLLoginChallenges) Consume(ctx context.Context, id, proofHash string, now time.Time) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE album_login_challenges SET consumed_at=? WHERE challenge_id=? AND proof_sha256=? AND consumed_at IS NULL AND expires_at>?`, now, id, proofHash, now)
	if err != nil {
		return "", err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", ErrLoginChallenge
	}
	var nonce string
	if err := tx.QueryRowContext(ctx, `SELECT apple_nonce FROM album_login_challenges WHERE challenge_id=?`, id).Scan(&nonce); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return nonce, nil
}

// VerifyAppleLogin consumes proof before contacting Apple. A failed or unknown
// exchange requires a new login, never reuse of a potentially consumed code.
// It returns an internal grant only; account/session issuance remains separate.
func (s *LoginChallengeService) VerifyAppleLogin(ctx context.Context, exchanger *AppleCodeExchanger, id, proof, code, identityToken string) (AppleGrant, error) {
	if exchanger == nil {
		return AppleGrant{}, ErrLoginChallenge
	}
	nonce, err := s.Consume(ctx, id, proof)
	if err != nil {
		return AppleGrant{}, ErrLoginChallenge
	}
	return exchanger.Exchange(ctx, code, identityToken, nonce)
}
