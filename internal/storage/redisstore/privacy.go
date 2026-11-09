package redisstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/quota"
)

type PrivacyCleaner struct {
	client *redis.Client
	prefix string
	appID  string
}

func NewPrivacyCleaner(client *redis.Client, appID string) *PrivacyCleaner {
	return &PrivacyCleaner{client: client, prefix: "platform:" + appID + ":", appID: appID}
}

func (cleaner *PrivacyCleaner) DeletePrincipal(ctx context.Context, principal attestation.Principal) error {
	if cleaner == nil || cleaner.client == nil {
		return nil
	}
	patterns := []string{
		cleaner.prefix + "quota:*:" + principal.DeviceID,
		cleaner.prefix + "quota:*:" + principal.DeviceID + ":*",
	}
	owners := make(map[string]struct{})
	for _, owner := range []string{principal.TransactionID, principal.KeyID, quota.FreeRecognitionTransactionPrefix + principal.KeyID} {
		if owner == "" || owner == quota.FreeRecognitionTransactionPrefix {
			continue
		}
		owners[owner] = struct{}{}
		patterns = append(patterns,
			cleaner.prefix+"quota:*:"+owner,
			cleaner.prefix+"quota:*:"+owner+":*",
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
	if err := cleaner.deleteMatching(ctx, cleaner.prefix+"quota:reservation:*", func(key string) (bool, error) {
		raw, err := cleaner.client.Get(ctx, key).Bytes()
		if err == redis.Nil {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var reservation quota.TokenReservation
		if json.Unmarshal(raw, &reservation) != nil {
			return false, nil
		}
		_, owned := owners[reservation.TransactionID]
		if !owned && principal.DeviceID != "" && reservation.DeviceID == principal.DeviceID && reservation.TransactionID != "" {
			// Keep the reservation until its historical billing counters have
			// been deleted, so a partial failure retains the retry reference.
			for _, pattern := range []string{
				cleaner.prefix + "quota:*:" + reservation.TransactionID,
				cleaner.prefix + "quota:*:" + reservation.TransactionID + ":*",
			} {
				if err := cleaner.deleteMatching(ctx, pattern, nil); err != nil {
					return false, err
				}
			}
			owners[reservation.TransactionID] = struct{}{}
			owned = true
		}
		return owned, nil
	}); err != nil {
		return err
	}
	if cleaner.appID != "" && principal.DeviceID != "" {
		if err := cleaner.client.Del(ctx, appRecognitionWindowKey(cleaner.appID, principal.DeviceID)).Err(); err != nil {
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
