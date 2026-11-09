package redisstore

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type CapabilityUseStore struct {
	client *redis.Client
	prefix string
}

func NewCapabilityUseStore(client *redis.Client, appID string) *CapabilityUseStore {
	return &CapabilityUseStore{client: client, prefix: "platform:" + appID + ":"}
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
	return store.client.SetNX(ctx, store.prefix+"job-capability:used:"+nonce, "1", ttl).Result()
}
