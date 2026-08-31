package redisstore

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
)

type PrivacyCleaner struct{ client *redis.Client }

func NewPrivacyCleaner(client *redis.Client) *PrivacyCleaner {
	return &PrivacyCleaner{client: client}
}

func (cleaner *PrivacyCleaner) DeletePrincipal(ctx context.Context, principal attestation.Principal) error {
	if cleaner == nil || cleaner.client == nil {
		return nil
	}
	patterns := []string{
		"health:quota:*:" + principal.DeviceID,
		"health:quota:*:" + principal.DeviceID + ":*",
	}
	if principal.TransactionID != "" {
		patterns = append(patterns,
			"health:quota:*:"+principal.TransactionID,
			"health:quota:*:"+principal.TransactionID+":*",
		)
	}
	for _, pattern := range patterns {
		if err := cleaner.deleteMatching(ctx, pattern, nil); err != nil {
			return err
		}
	}
	return cleaner.deleteMatching(ctx, "health:attest:nonce:*", func(key string) (bool, error) {
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
			if !strings.HasPrefix(key, "health:") {
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
