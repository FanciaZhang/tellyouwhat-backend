package redisstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/quota"
)

var deferLegacyJobScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw or raw ~= ARGV[1] then return 0 end
local value = cjson.decode(raw)
if value.version ~= 1 or value.reconciled == true or (value.attempt and value.attempt ~= 0) or value.rootReservationID then return 0 end
if ARGV[2] ~= '1' then return value.reservedTokens end
for index = 2, 3 do
  if redis.call('EXISTS', KEYS[index]) == 1 then
    local used = math.max(0, tonumber(redis.call('GET', KEYS[index])) - value.reservedTokens)
    redis.call('SET', KEYS[index], used, 'KEEPTTL')
  end
end
value.version, value.charged = 2, false
redis.call('SET', KEYS[1], cjson.encode(value), 'KEEPTTL')
return value.reservedTokens
`)

// DeferLegacyJob converts only an unclaimed capability prepayment. The caller
// derives reservationID from a durable authenticated request binding. Workers
// atomically mark attempt ownership before any provider call, so a concurrent
// claim prevents conversion. Version 2 requires an upgraded worker to debit it.
func (limiter *QuotaLimiter) DeferLegacyJob(ctx context.Context, reservationID string, apply bool) (int, error) {
	if limiter == nil || limiter.client == nil || reservationID == "" {
		return 0, quota.ErrInvalidReservation
	}
	key := limiter.prefix + "quota:reservation:" + reservationID
	raw, err := limiter.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var value quota.TokenReservation
	if json.Unmarshal(raw, &value) != nil || !value.Matches(value.TransactionID, value.ReservedTokens) || value.Version != 1 || value.Reconciled || value.Attempt != 0 || value.RootReservationID != "" {
		return 0, nil
	}
	mode := "0"
	if apply {
		mode = "1"
	}
	return deferLegacyJobScript.Run(ctx, limiter.client, []string{key, limiter.prefix + "quota:day:" + value.DailyWindow + ":" + value.TransactionID, limiter.prefix + "quota:month:" + value.MonthlyWindow + ":" + value.TransactionID}, string(raw), mode).Int()
}
