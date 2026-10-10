package albums

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestLoginLimitCanonicalAddressAndPrivacy(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	secret := bytes.Repeat([]byte{7}, 32)
	limiter, err := NewRedisLoginLimiter(client, secret, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secret[0] = 99
	for _, pair := range [][2]string{{"192.0.2.1", "::ffff:192.0.2.1"}, {"2001:db8:1:2::1", "2001:db8:1:2::abcd"}} {
		a, _ := limiter.addressKey("login", pair[0])
		b, err := limiter.addressKey("login", pair[1])
		if err != nil || a != b {
			t.Fatal("same network has separate limits")
		}
		if strings.Contains(a, pair[0]) {
			t.Fatal("raw address persisted")
		}
	}
	a, _ := limiter.addressKey("login", "192.0.2.1")
	b, _ := limiter.addressKey("challenge", "192.0.2.1")
	if a == b {
		t.Fatal("operations share budget")
	}
	for _, address := range []string{"", "not-an-ip", "fe80::1%en0", "192.0.2.1:1234"} {
		if _, err := limiter.addressKey("login", address); err == nil {
			t.Fatal("bad address accepted")
		}
	}
	if _, err := limiter.Check(context.Background(), "other", "192.0.2.1"); err == nil {
		t.Fatal("bad operation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Check(ctx, "login", "192.0.2.1"); !errors.Is(err, ErrLoginLimitUnavailable) {
		t.Fatal("unavailable limiter failed open")
	}
	if _, err := NewRedisLoginLimiter(client, nil, 10, time.Minute); err == nil {
		t.Fatal("missing privacy key accepted")
	}
}

func TestRedisAlbumLoginLimitAcrossInstances(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("set REDIS_TEST_URL for real login limiter integration")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prefix := "albums:login-limit-test:" + uuid.NewString() + ":"
	secret := bytes.Repeat([]byte{3}, 32)
	limiter, _ := NewRedisLoginLimiter(client, secret, 7, time.Minute)
	limiter.prefix = prefix
	key, _ := limiter.addressKey("login", "192.0.2.1")
	defer client.Del(context.Background(), key)
	var accepted atomic.Int32
	var group sync.WaitGroup
	for range 30 {
		group.Add(1)
		go func() {
			defer group.Done()
			instance, _ := NewRedisLoginLimiter(client, secret, 7, time.Minute)
			instance.prefix = prefix
			delay, err := instance.Check(ctx, "login", "192.0.2.1")
			if err != nil {
				t.Error(err)
				return
			}
			if delay == 0 {
				accepted.Add(1)
			} else if delay > time.Minute {
				t.Error("invalid retry delay")
			}
		}()
	}
	group.Wait()
	if accepted.Load() != 7 {
		t.Fatalf("accepted %d of 30; expected 7", accepted.Load())
	}
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatal("missing expiry", ttl, err)
	}
	// Force expiry without sleeping, then verify a fresh window is accepted.
	if err := client.PExpireAt(ctx, key, time.Now().Add(-time.Second)).Err(); err != nil {
		t.Fatal(err)
	}
	if delay, err := limiter.Check(ctx, "login", "192.0.2.1"); err != nil || delay != 0 {
		t.Fatal("expired budget not reset", delay, err)
	}
	if err := client.Persist(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.Check(ctx, "login", "192.0.2.1"); !errors.Is(err, ErrLoginLimitUnavailable) {
		t.Fatal("corrupt expiry failed open")
	}
}
