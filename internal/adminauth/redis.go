package adminauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStateStore struct {
	client *redis.Client
}

func NewRedisStateStore(client *redis.Client) *RedisStateStore {
	return &RedisStateStore{client: client}
}

func (store *RedisStateStore) PutCeremony(
	ctx context.Context,
	id string,
	state CeremonyState,
	ttl time.Duration,
) error {
	return store.put(ctx, "admin:ceremony:"+id, state, ttl)
}

func (store *RedisStateStore) TakeCeremony(ctx context.Context, id string) (CeremonyState, bool, error) {
	var state CeremonyState
	found, err := store.getDel(ctx, "admin:ceremony:"+id, &state)
	return state, found, err
}

func (store *RedisStateStore) PutSession(
	ctx context.Context,
	hash [32]byte,
	session Session,
	ttl time.Duration,
) error {
	return store.put(ctx, sessionKey(hash), session, ttl)
}

func (store *RedisStateStore) GetSession(ctx context.Context, hash [32]byte) (Session, bool, error) {
	var session Session
	value, err := store.client.Get(ctx, sessionKey(hash)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if err := json.Unmarshal(value, &session); err != nil {
		return Session{}, false, err
	}
	return session, true, nil
}

func (store *RedisStateStore) DeleteSession(ctx context.Context, hash [32]byte) error {
	return store.client.Del(ctx, sessionKey(hash)).Err()
}

func (store *RedisStateStore) put(ctx context.Context, key string, value any, ttl time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return store.client.Set(ctx, key, encoded, ttl).Err()
}

func (store *RedisStateStore) getDel(ctx context.Context, key string, destination any) (bool, error) {
	value, err := store.client.GetDel(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(value, destination); err != nil {
		return false, err
	}
	return true, nil
}

func sessionKey(hash [32]byte) string {
	return "admin:session:" + hex.EncodeToString(hash[:])
}

var _ StateStore = (*RedisStateStore)(nil)
