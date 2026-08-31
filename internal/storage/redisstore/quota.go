package redisstore

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/quota"
)

type QuotaLimiter struct {
	client *redis.Client
	limits quota.Limits
	prefix string
}

const concurrencyLeaseTTL = 16 * time.Minute

func NewQuotaLimiter(client *redis.Client, limits quota.Limits, appID string) *QuotaLimiter {
	return &QuotaLimiter{client: client, limits: limits, prefix: "platform:" + appID + ":"}
}

var acquireQuotaScript = redis.NewScript(`
local function current(key)
  return tonumber(redis.call('GET', key) or '0')
end
local ipLimit = tonumber(ARGV[1])
local deviceLimit = tonumber(ARGV[2])
local operationLimit = tonumber(ARGV[3])
local dayLimit = tonumber(ARGV[4])
local monthLimit = tonumber(ARGV[5])
local concurrentLimit = tonumber(ARGV[6])
local tokens = tonumber(ARGV[7])
local reservationEnabled = ARGV[11] ~= ''
if reservationEnabled and redis.call('EXISTS', KEYS[7]) == 1 then return 2 end
if ipLimit > 0 and current(KEYS[1]) + 1 > ipLimit then return 10 end
if deviceLimit > 0 and current(KEYS[2]) + 1 > deviceLimit then return 11 end
if operationLimit > 0 and current(KEYS[3]) + 1 > operationLimit then return 12 end
if dayLimit > 0 and current(KEYS[4]) + tokens > dayLimit then return 13 end
if monthLimit > 0 and current(KEYS[5]) + tokens > monthLimit then return 14 end
if concurrentLimit > 0 and current(KEYS[6]) + 1 > concurrentLimit then return 15 end
for index = 1, 3 do
  local value = redis.call('INCR', KEYS[index])
  if value == 1 then redis.call('EXPIRE', KEYS[index], tonumber(ARGV[8])) end
end
local dayValue = redis.call('INCRBY', KEYS[4], tokens)
if dayValue == tokens then redis.call('EXPIRE', KEYS[4], tonumber(ARGV[9])) end
local monthValue = redis.call('INCRBY', KEYS[5], tokens)
if monthValue == tokens then redis.call('EXPIRE', KEYS[5], tonumber(ARGV[10])) end
local concurrentValue = redis.call('INCR', KEYS[6])
if concurrentValue == 1 then redis.call('EXPIRE', KEYS[6], tonumber(ARGV[13])) end
if reservationEnabled then redis.call('SET', KEYS[7], '1', 'EX', tonumber(ARGV[12])) end
return 1
`)

func (limiter *QuotaLimiter) Acquire(
	ctx context.Context,
	identity quota.Identity,
	operation contracts.Operation,
	estimatedTokens int,
	reservationID string,
	now time.Time,
) (quota.Releaser, error) {
	if limiter == nil || limiter.client == nil || identity.DeviceID == "" || identity.TransactionID == "" || identity.IP == "" || estimatedTokens < 0 {
		return nil, quota.ErrInvalidIdentity
	}
	minute := now.UTC().Format("200601021504")
	day := now.UTC().Format("20060102")
	month := now.UTC().Format("200601")
	keys := []string{
		limiter.prefix + "quota:minute:ip:" + minute + ":" + identity.IP,
		limiter.prefix + "quota:minute:device:" + minute + ":" + identity.DeviceID,
		limiter.prefix + "quota:minute:operation:" + minute + ":" + identity.TransactionID + ":" + string(operation),
		limiter.prefix + "quota:day:" + day + ":" + identity.TransactionID,
		limiter.prefix + "quota:month:" + month + ":" + identity.TransactionID,
		limiter.prefix + "quota:concurrent:" + identity.DeviceID,
		limiter.prefix + "quota:reservation:" + reservationID,
	}
	nextDay := now.UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)
	nextMonth := time.Date(now.UTC().Year(), now.UTC().Month()+1, 1, 1, 0, 0, 0, time.UTC)
	result, err := acquireQuotaScript.Run(ctx, limiter.client, keys,
		limiter.limits.RequestsPerMinutePerIP,
		limiter.limits.RequestsPerMinutePerDevice,
		limiter.limits.RequestsPerMinutePerOperation,
		limiter.limits.DailyTokensPerTransaction,
		limiter.limits.MonthlyTokensPerTransaction,
		limiter.limits.MaxConcurrentPerDevice,
		estimatedTokens,
		120,
		int(nextDay.Sub(now.UTC()).Seconds()),
		int(nextMonth.Sub(now.UTC()).Seconds()),
		reservationID,
		int((25 * time.Hour).Seconds()),
		int(concurrencyLeaseTTL.Seconds()),
	).Int()
	if err != nil {
		return nil, err
	}
	if result == 2 {
		return &redisQuotaLease{}, nil
	}
	if result != 1 {
		scopes := map[int]quota.LimitScope{
			10: quota.LimitRequestsPerMinuteIP,
			11: quota.LimitRequestsPerMinuteDevice,
			12: quota.LimitRequestsPerMinuteOperation,
			13: quota.LimitDailyTokens,
			14: quota.LimitMonthlyTokens,
			15: quota.LimitConcurrentDevice,
		}
		if scope, ok := scopes[result]; ok {
			return nil, quota.Exceeded(scope)
		}
		return nil, quota.ErrExceeded
	}
	return &redisQuotaLease{
		client:      limiter.client,
		dayKey:      keys[3],
		monthKey:    keys[4],
		concurrent:  keys[5],
		reservation: keys[6],
		reserved:    estimatedTokens,
		owned:       true,
	}, nil
}

var releaseQuotaScript = redis.NewScript(`
local concurrent = tonumber(redis.call('DECR', KEYS[3]) or '0')
if concurrent <= 0 then redis.call('DEL', KEYS[3]) end
local delta = tonumber(ARGV[1])
if delta ~= 0 then
  redis.call('INCRBY', KEYS[1], delta)
  redis.call('INCRBY', KEYS[2], delta)
end
if ARGV[2] == '1' then redis.call('DEL', KEYS[4]) end
return 1
`)

type redisQuotaLease struct {
	once        sync.Once
	client      *redis.Client
	dayKey      string
	monthKey    string
	concurrent  string
	reservation string
	reserved    int
	owned       bool
}

func (lease *redisQuotaLease) Release(actualTokens int) {
	if lease == nil || !lease.owned || lease.client == nil {
		return
	}
	lease.once.Do(func() {
		if actualTokens < 0 {
			actualTokens = 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		deleteReservation := "0"
		if actualTokens == 0 {
			deleteReservation = "1"
		}
		_ = releaseQuotaScript.Run(
			ctx,
			lease.client,
			[]string{lease.dayKey, lease.monthKey, lease.concurrent, lease.reservation},
			strconv.Itoa(actualTokens-lease.reserved),
			deleteReservation,
		).Err()
	})
}

var _ quota.Releaser = (*redisQuotaLease)(nil)

var reconcileTokensScript = redis.NewScript(`
local function adjust(key, delta)
  if redis.call('EXISTS', key) == 0 then return end
  local value = tonumber(redis.call('GET', key) or '0') + delta
  if value < 0 then value = 0 end
  redis.call('SET', key, value, 'KEEPTTL')
end
adjust(KEYS[1], tonumber(ARGV[1]))
adjust(KEYS[2], tonumber(ARGV[1]))
return 1
`)

func (limiter *QuotaLimiter) Reconcile(
	ctx context.Context,
	transactionID string,
	reserved,
	actual int,
	now time.Time,
) error {
	if limiter == nil || limiter.client == nil || transactionID == "" || reserved < 0 || actual < 0 {
		return quota.ErrInvalidIdentity
	}
	day := now.UTC().Format("20060102")
	month := now.UTC().Format("200601")
	return reconcileTokensScript.Run(ctx, limiter.client, []string{
		limiter.prefix + "quota:day:" + day + ":" + transactionID,
		limiter.prefix + "quota:month:" + month + ":" + transactionID,
	}, actual-reserved).Err()
}

func (limiter *QuotaLimiter) Snapshot(
	ctx context.Context,
	transactionID string,
	now time.Time,
) (quota.Snapshot, error) {
	if limiter == nil || limiter.client == nil || transactionID == "" {
		return quota.Snapshot{}, quota.ErrInvalidIdentity
	}
	utc := now.UTC()
	values, err := limiter.client.MGet(ctx,
		limiter.prefix+"quota:day:"+utc.Format("20060102")+":"+transactionID,
		limiter.prefix+"quota:month:"+utc.Format("200601")+":"+transactionID,
	).Result()
	if err != nil {
		return quota.Snapshot{}, err
	}
	parse := func(value any) int {
		parsed, _ := strconv.Atoi(fmt.Sprint(value))
		return parsed
	}
	return quota.Snapshot{
		DailyUsed:      parse(values[0]),
		DailyLimit:     limiter.limits.DailyTokensPerTransaction,
		DailyResetAt:   utc.Truncate(24 * time.Hour).Add(24 * time.Hour),
		MonthlyUsed:    parse(values[1]),
		MonthlyLimit:   limiter.limits.MonthlyTokensPerTransaction,
		MonthlyResetAt: time.Date(utc.Year(), utc.Month()+1, 1, 0, 0, 0, 0, time.UTC),
	}, nil
}

var _ quota.TokenReconciler = (*QuotaLimiter)(nil)
var _ quota.Reader = (*QuotaLimiter)(nil)
