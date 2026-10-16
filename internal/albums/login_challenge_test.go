package albums

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Memory store is only a service test double. SQL concurrency has its own test.
type challengeTestStore struct {
	mu   sync.Mutex
	rows map[string]StoredLoginChallenge
	used map[string]bool
}

func (r *challengeTestStore) Insert(_ context.Context, c StoredLoginChallenge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[c.ID] = c
	return nil
}
func (r *challengeTestStore) Consume(_ context.Context, id, hash string, now time.Time) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.rows[id]
	if !ok || c.ProofSHA256 != hash || r.used[id] || !now.Before(c.ExpiresAt) {
		return "", ErrLoginChallenge
	}
	r.used[id] = true
	return c.Nonce, nil
}
func TestLoginChallengeProofIsHashedAndSingleUse(t *testing.T) {
	now := time.Unix(1791660000, 0)
	store := &challengeTestStore{rows: map[string]StoredLoginChallenge{}, used: map[string]bool{}}
	service, _ := NewLoginChallengeService(store, func() time.Time { return now })
	challenge, err := service.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !challenge.ExpiresAt.Equal(now.Add(5*time.Minute)) || !loginNoncePattern.MatchString(challenge.Nonce) {
		t.Fatal("invalid challenge binding")
	}
	stored := store.rows[challenge.ID]
	raw, _ := base64.RawURLEncoding.DecodeString(challenge.Proof)
	hash := sha256.Sum256(raw)
	if stored.ProofSHA256 != hex.EncodeToString(hash[:]) || stored.ProofSHA256 == challenge.Proof {
		t.Fatal("raw proof stored")
	}
	if fmt.Sprintf("%+v", challenge) != "[redacted album login challenge]" {
		t.Fatal("proof logging is not redacted")
	}
	if _, err := service.Consume(context.Background(), challenge.ID, base64.RawURLEncoding.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("wrong proof accepted")
	}
	nonce, err := service.Consume(context.Background(), challenge.ID, challenge.Proof)
	if err != nil || nonce != challenge.Nonce {
		t.Fatal("valid challenge failed", err)
	}
	if _, err := service.Consume(context.Background(), challenge.ID, challenge.Proof); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("replayed challenge accepted")
	}
	expired, err := service.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = expired.ExpiresAt
	if _, err := service.Consume(context.Background(), expired.ID, expired.Proof); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("expiry boundary accepted")
	}
}
