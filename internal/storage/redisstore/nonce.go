package redisstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
)

type NonceStore struct{ client *redis.Client }

func NewNonceStore(client *redis.Client) *NonceStore {
	return &NonceStore{client: client}
}

func (store *NonceStore) Issue(
	ctx context.Context,
	keyID string,
	ttl time.Duration,
	_ time.Time,
) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(random)
	if err := store.client.Set(ctx, "health:attest:nonce:"+nonce, keyID, ttl).Err(); err != nil {
		return "", err
	}
	return nonce, nil
}

var consumeNonceScript = redis.NewScript(`
local value = redis.call('GET', KEYS[1])
if not value then
  return -1
end
if value == 'used:' .. ARGV[1] then
  return 0
end
if value ~= ARGV[1] then return -1 end
local ttl = redis.call('PTTL', KEYS[1])
if ttl <= 0 then
  return -1
end
redis.call('SET', KEYS[1], 'used:' .. ARGV[1], 'PX', ttl)
return 1
`)

func (store *NonceStore) Consume(
	ctx context.Context,
	nonce string,
	keyID string,
	_ time.Time,
) error {
	result, err := consumeNonceScript.Run(
		ctx,
		store.client,
		[]string{"health:attest:nonce:" + nonce},
		keyID,
	).Int()
	if err != nil {
		return err
	}
	switch result {
	case 1:
		return nil
	case 0:
		return attestation.ErrReplay
	default:
		return attestation.ErrAuthentication
	}
}

var _ attestation.NonceStore = (*NonceStore)(nil)

