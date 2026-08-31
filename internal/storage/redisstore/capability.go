package redisstore

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type CapabilityUseStore struct{ client *redis.Client }

func NewCapabilityUseStore(client *redis.Client) *CapabilityUseStore {
	return &CapabilityUseStore{client: client}
}

func (store *CapabilityUseStore) Consume(
	ctx context.Context,
	nonce string,
	expiresAt time.Time,
	now time.Time,
) (bool, error) {
	ttl := expiresAt.Sub(now)
	if ttl <= 0 {
		return false, nil
	}
	return store.client.SetNX(ctx, "health:job-capability:used:"+nonce, "1", ttl).Result()
}

