package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/quota"
)

var reserveJobAttemptScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
if not raw then return 0 end
local ok, root = pcall(cjson.decode, raw)
local tokens, attempt = tonumber(ARGV[1]), tonumber(ARGV[4])
if not ok or type(root) ~= 'table' or root.version ~= 1 or
   root.transactionID ~= ARGV[2] or root.deviceID ~= ARGV[3] or root.reservedTokens ~= tokens or
   root.dailyWindow ~= ARGV[13] or root.monthlyWindow ~= ARGV[14] or
   (root.rootReservationID and root.rootReservationID ~= ARGV[5]) or
   (root.attempt and root.attempt ~= 1) then return 0 end
if attempt == 1 then
  if root.reconciled == true then return 0 end
  root.rootReservationID, root.attempt = ARGV[5], 1
  redis.call('SET', KEYS[1], cjson.encode(root), 'KEEPTTL')
  return 1
end
local previous = redis.call('GET', KEYS[2])
if previous then
  local valid, value = pcall(cjson.decode, previous)
  if not valid or type(value) ~= 'table' or value.version ~= 1 or
     value.transactionID ~= ARGV[2] or value.deviceID ~= ARGV[3] or value.reservedTokens ~= tokens or
     value.rootReservationID ~= ARGV[5] or value.attempt ~= attempt or value.reconciled == true then return 0 end
  return 1
end
local dayUsed = tonumber(redis.call('GET', KEYS[3]) or '0')
local monthUsed = tonumber(redis.call('GET', KEYS[4]) or '0')
local dayLimit, monthLimit = tonumber(ARGV[8]), tonumber(ARGV[9])
if dayLimit > 0 and tokens > dayLimit - dayUsed then return 13 end
if monthLimit > 0 and tokens > monthLimit - monthUsed then return 14 end
local dayValue = redis.call('INCRBY', KEYS[3], tokens)
if dayValue == tokens then redis.call('EXPIRE', KEYS[3], tonumber(ARGV[10])) end
local monthValue = redis.call('INCRBY', KEYS[4], tokens)
if monthValue == tokens then redis.call('EXPIRE', KEYS[4], tonumber(ARGV[11])) end
local reservation = cjson.encode({version=1, transactionID=ARGV[2], deviceID=ARGV[3],
  dailyWindow=ARGV[6], monthlyWindow=ARGV[7], reservedTokens=tokens, reconciled=false,
  rootReservationID=ARGV[5], attempt=attempt})
redis.call('SET', KEYS[2], reservation, 'EX', tonumber(ARGV[12]))
return 1
`)

func (limiter *QuotaLimiter) ReserveJobAttempt(ctx context.Context, attempt quota.JobAttempt, now time.Time) (string, error) {
	if limiter == nil || limiter.client == nil || !attempt.Valid() {
		return "", quota.ErrInvalidReservation
	}
	rootKey := limiter.prefix + "quota:reservation:" + attempt.ReservationID
	raw, err := limiter.client.Get(ctx, rootKey).Bytes()
	if errors.Is(err, redis.Nil) {
		return "", quota.ErrInvalidReservation
	}
	if err != nil {
		return "", err
	}
	var root quota.TokenReservation
	if json.Unmarshal(raw, &root) != nil || !root.Matches(attempt.TransactionID, attempt.ReservedTokens) || root.DeviceID != attempt.DeviceID {
		return "", quota.ErrInvalidReservation
	}
	utc := now.UTC()
	day, month := utc.Format("20060102"), utc.Format("200601")
	nextDay := utc.Truncate(24 * time.Hour).Add(25 * time.Hour)
	nextMonth := time.Date(utc.Year(), utc.Month()+1, 1, 1, 0, 0, 0, time.UTC)
	id := attempt.TokenReservationID()
	result, err := reserveJobAttemptScript.Run(ctx, limiter.client, []string{
		rootKey, limiter.prefix + "quota:reservation:" + id,
		limiter.prefix + "quota:day:" + day + ":" + attempt.TransactionID,
		limiter.prefix + "quota:month:" + month + ":" + attempt.TransactionID,
	}, attempt.ReservedTokens, attempt.TransactionID, attempt.DeviceID, attempt.Number, attempt.ReservationID,
		day, month, limiter.limits.DailyTokensPerTransaction, limiter.limits.MonthlyTokensPerTransaction,
		int(nextDay.Sub(utc).Seconds()), int(nextMonth.Sub(utc).Seconds()), int((25 * time.Hour).Seconds()),
		root.DailyWindow, root.MonthlyWindow).Int()
	if err != nil {
		return "", err
	}
	switch result {
	case 1:
		return id, nil
	case 13:
		return "", quota.Exceeded(quota.LimitDailyTokens)
	case 14:
		return "", quota.Exceeded(quota.LimitMonthlyTokens)
	default:
		return "", quota.ErrInvalidReservation
	}
}

var _ quota.JobAttemptBudget = (*QuotaLimiter)(nil)
