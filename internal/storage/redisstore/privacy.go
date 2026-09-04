package redisstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
)

type PrivacyCleaner struct {
	client *redis.Client
	prefix string
}

func NewPrivacyCleaner(client *redis.Client, appID string) *PrivacyCleaner {
	return &PrivacyCleaner{client: client, prefix: "platform:" + appID + ":"}
}

func (cleaner *PrivacyCleaner) DeletePrincipal(ctx context.Context, principal attestation.Principal) error {
	if cleaner == nil || cleaner.client == nil {
		return nil
	}
	patterns := []string{
		cleaner.prefix + "quota:*:" + principal.DeviceID,
		cleaner.prefix + "quota:*:" + principal.DeviceID + ":*",
	}
	if principal.TransactionID != "" {
		patterns = append(patterns,
			cleaner.prefix+"quota:*:"+principal.TransactionID,
			cleaner.prefix+"quota:*:"+principal.TransactionID+":*",
		)
	}
	if cleaner.prefix == "platform:journal:" {
		for _, environment := range []string{"production", "sandbox", "development"} {
			owner := principal.TransactionID
			if owner == "" {
				owner = principal.KeyID
			}
			sum := sha256.Sum256([]byte(environment + ":" + owner))
			patterns = append(patterns, cleaner.prefix+"voice:{"+hex.EncodeToString(sum[:])+"}:*")
		}
	}
	for _, pattern := range patterns {
		if err := cleaner.deleteMatching(ctx, pattern, nil); err != nil {
			return err
		}
	}
	return cleaner.deleteMatching(ctx, cleaner.prefix+"attest:nonce:*", func(key string) (bool, error) {
		value, err := cleaner.client.Get(ctx, key).Result()
		if err == redis.Nil {
			return false, nil
		}
		return value == principal.KeyID || value == "used:"+principal.KeyID, err
	})
}

func (cleaner *PrivacyCleaner) deleteMatching(ctx context.Context, pattern string, include func(string) (bool, error)) error {
	var cursor uint64
	for {
		keys, next, err := cleaner.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, cleaner.prefix) {
				continue
			}
			allowed := true
			if include != nil {
				allowed, err = include(key)
				if err != nil {
					return err
				}
			}
			if allowed {
				if err := cleaner.client.Del(ctx, key).Err(); err != nil {
					return err
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}
