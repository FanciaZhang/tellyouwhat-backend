package albums

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrLoginLimitUnavailable = errors.New("album login rate limiter unavailable")

type LoginRateLimiter interface {
	// Check returns a nonzero retry delay when the request is denied.
	Check(context.Context, string, string) (time.Duration, error)
}

type RedisLoginLimiter struct {
	client *redis.Client
	key    []byte
	prefix string
	limit  int
	window time.Duration
}

func NewRedisLoginLimiter(client *redis.Client, key []byte, limit int, window time.Duration) (*RedisLoginLimiter, error) {
	if client == nil || len(key) != 32 || limit < 1 || limit > 1000 || window < time.Second || window > time.Hour {
		return nil, errors.New("invalid album login rate limit configuration")
	}
	return &RedisLoginLimiter{client: client, key: append([]byte(nil), key...), prefix: "albums:login-limit:v1:", limit: limit, window: window}, nil
}

// Atomic fixed-duration window starting on the first request. Refused requests
// do not extend the window. Redis failures deny requests rather than bypassing.
var albumLoginLimitScript = redis.NewScript(`
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local ttl = redis.call('PTTL', KEYS[1])
if current > 0 and ttl <= 0 then return redis.error_reply('invalid limiter expiry') end
if current >= tonumber(ARGV[1]) then return ttl end
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 0
`)

func (l *RedisLoginLimiter) addressKey(operation, address string) (string, error) {
	if operation != "challenge" && operation != "login" && operation != "logout" && operation != "subscription" && operation != "notification" && operation != "apple-notification" && operation != "share-owner" && operation != "share-read" && operation != "share-claim" && operation != "push-registration" {
		return "", ErrLoginLimitUnavailable
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || ip.Zone() != "" {
		return "", ErrLoginLimitUnavailable
	}
	ip = ip.Unmap()
	scope := ip.String()
	// Privacy addresses on the same IPv6 subnet share a budget.
	if ip.Is6() {
		scope = netip.PrefixFrom(ip, 64).Masked().String()
	}
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(operation + "\x00" + scope))
	return l.prefix + hex.EncodeToString(mac.Sum(nil)), nil
}
func (l *RedisLoginLimiter) Check(ctx context.Context, operation, address string) (time.Duration, error) {
	key, err := l.addressKey(operation, address)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	milliseconds, err := albumLoginLimitScript.Run(ctx, l.client, []string{key}, l.limit, l.window.Milliseconds()).Int64()
	if err != nil || milliseconds < 0 {
		return 0, ErrLoginLimitUnavailable
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

var _ LoginRateLimiter = (*RedisLoginLimiter)(nil)
