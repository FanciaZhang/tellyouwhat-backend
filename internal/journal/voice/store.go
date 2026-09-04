package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Receipts retain transcript text for at most 24 hours. Quota counters contain
// no journal content. Each connection holds a subscription-wide fenced lease.
type Store interface {
	Lock(context.Context, string, string) error
	Renew(context.Context, string, string) error
	Unlock(context.Context, string, string)
	Receipt(context.Context, string, string, string) (*Receipt, error)
	Commit(context.Context, string, string, string, string, Receipt, int) (int, error)
	Remaining(context.Context, string, string, int) (int, error)
	Forget(context.Context, string, string) error
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type memoryReceipt struct {
	Receipt Receipt
	Until   time.Time
}
type memoryLease struct {
	Token string
	Until time.Time
}
type MemoryStore struct {
	mu          sync.Mutex
	receipts    map[string]memoryReceipt
	leases      map[string]memoryLease
	used        map[string]int
	sessionUsed map[string]int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{receipts: map[string]memoryReceipt{}, leases: map[string]memoryLease{}, used: map[string]int{}, sessionUsed: map[string]int{}}
}
func (s *MemoryStore) Lock(_ context.Context, owner, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.leases[owner]; x.Until.After(time.Now()) {
		return ErrBusy
	}
	s.leases[owner] = memoryLease{token, time.Now().Add(45 * time.Second)}
	return nil
}
func (s *MemoryStore) Renew(_ context.Context, owner, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.leases[owner]
	if x.Token != token || !x.Until.After(time.Now()) {
		return ErrBusy
	}
	s.leases[owner] = memoryLease{token, time.Now().Add(45 * time.Second)}
	return nil
}
func (s *MemoryStore) Unlock(_ context.Context, owner, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[owner].Token == token {
		delete(s.leases, owner)
	}
}
func (s *MemoryStore) Receipt(_ context.Context, owner, session, segment string) (*Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + session + segment
	if r, ok := s.receipts[key]; ok {
		if r.Until.After(time.Now()) {
			return &r.Receipt, nil
		}
		delete(s.receipts, key)
	}
	return nil, nil
}
func (s *MemoryStore) Remaining(_ context.Context, owner, period string, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return max(0, limit-s.used[owner+period]), nil
}
func (s *MemoryStore) Commit(_ context.Context, owner, session, period, token string, r Receipt, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + session + r.SegmentID
	if lease := s.leases[owner]; lease.Token != token || !lease.Until.After(time.Now()) {
		return 0, ErrBusy
	}
	if old, ok := s.receipts[key]; ok && old.Until.After(time.Now()) {
		if old.Receipt.SHA256 != r.SHA256 {
			return 0, ErrConflict
		}
		return max(0, limit-s.used[owner+period]), nil
	}
	if s.used[owner+period]+r.Milliseconds > limit || s.sessionUsed[owner+session]+r.Milliseconds > SessionMilliseconds {
		return 0, ErrQuota
	}
	s.used[owner+period] += r.Milliseconds
	s.sessionUsed[owner+session] += r.Milliseconds
	s.receipts[key] = memoryReceipt{r, time.Now().Add(24 * time.Hour)}
	return max(0, limit-s.used[owner+period]), nil
}

type Cipher interface {
	Encrypt([]byte, []byte) ([]byte, []byte, error)
	Decrypt([]byte, []byte, []byte) ([]byte, error)
}
type RedisStore struct {
	Client *redis.Client
	Cipher Cipher
}

func (s RedisStore) key(owner, suffix string) string {
	return "platform:journal:voice:{" + hash(owner) + "}:" + suffix
}
func (s RedisStore) Lock(ctx context.Context, owner, token string) error {
	ok, err := s.Client.SetNX(ctx, s.key(owner, "lease"), token, 45*time.Second).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrBusy
	}
	return nil
}
func (s RedisStore) Renew(ctx context.Context, owner, token string) error {
	v, err := s.Client.Eval(ctx, `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end; redis.call('EXPIRE',KEYS[1],45); return 1`, []string{s.key(owner, "lease")}, token).Int()
	if err != nil {
		return err
	}
	if v != 1 {
		return ErrBusy
	}
	return nil
}
func (s RedisStore) Unlock(ctx context.Context, owner, token string) {
	s.Client.Eval(ctx, `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`, []string{s.key(owner, "lease")}, token)
}

type sealedReceipt struct{ Ciphertext, Nonce []byte }

func (s RedisStore) Receipt(ctx context.Context, owner, session, segment string) (*Receipt, error) {
	key := s.key(owner, "receipt:"+session+":"+segment)
	raw, err := s.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sealed sealedReceipt
	if err = json.Unmarshal(raw, &sealed); err != nil {
		return nil, err
	}
	plain, err := s.Cipher.Decrypt(sealed.Ciphertext, sealed.Nonce, []byte(key))
	if err != nil {
		return nil, err
	}
	var receipt Receipt
	err = json.Unmarshal(plain, &receipt)
	return &receipt, err
}
func (s RedisStore) Remaining(ctx context.Context, owner, period string, limit int) (int, error) {
	used, err := s.Client.Get(ctx, s.key(owner, "quota:"+period)).Int()
	if errors.Is(err, redis.Nil) {
		return limit, nil
	}
	return max(0, limit-used), err
}
func (s RedisStore) Commit(ctx context.Context, owner, session, period, token string, r Receipt, limit int) (int, error) {
	receiptKey := s.key(owner, "receipt:"+session+":"+r.SegmentID)
	if old, err := s.Receipt(ctx, owner, session, r.SegmentID); err != nil {
		return 0, err
	} else if old != nil {
		if old.SHA256 != r.SHA256 {
			return 0, ErrConflict
		}
		return s.Remaining(ctx, owner, period, limit)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	ciphertext, nonce, err := s.Cipher.Encrypt(raw, []byte(receiptKey))
	if err != nil {
		return 0, err
	}
	raw, err = json.Marshal(sealedReceipt{ciphertext, nonce})
	if err != nil {
		return 0, err
	}
	value, err := s.Client.Eval(ctx, `
 if redis.call('GET',KEYS[1])~=ARGV[1] then return -2 end
 local used=tonumber(redis.call('GET',KEYS[2]) or '0')
 if redis.call('EXISTS',KEYS[4])==1 then return tonumber(ARGV[3])-used end
 local duration=tonumber(redis.call('GET',KEYS[3]) or '0')
 if used+tonumber(ARGV[2])>tonumber(ARGV[3]) or duration+tonumber(ARGV[2])>1800000 then return -1 end
 redis.call('INCRBY',KEYS[2],ARGV[2]); redis.call('EXPIRE',KEYS[2],8035200)
 redis.call('INCRBY',KEYS[3],ARGV[2]); redis.call('EXPIRE',KEYS[3],86400)
 redis.call('SET',KEYS[4],ARGV[4],'EX',86400)
 return tonumber(ARGV[3])-used-tonumber(ARGV[2])`, []string{s.key(owner, "lease"), s.key(owner, "quota:"+period), s.key(owner, "duration:"+session), receiptKey}, token, r.Milliseconds, limit, string(raw)).Int()
	if err != nil {
		return 0, err
	}
	if value == -1 {
		return 0, ErrQuota
	}
	if value == -2 {
		return 0, ErrBusy
	}
	return value, nil
}

// Forget removes transcript content after the client has durably acknowledged
// the final document. Non-content hashes remain for retry-safe billing.
func (s *MemoryStore) Forget(_ context.Context, owner, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.receipts {
		if strings.HasPrefix(key, owner+session) {
			value.Receipt.Text = ""
			s.receipts[key] = value
		}
	}
	return nil
}
func (s RedisStore) Forget(ctx context.Context, owner, session string) error {
	var cursor uint64
	for {
		keys, next, err := s.Client.Scan(ctx, cursor, s.key(owner, "receipt:"+session+":*"), 100).Result()
		if err != nil {
			return err
		}
		for _, key := range keys {
			segment := strings.TrimPrefix(key, s.key(owner, "receipt:"+session+":"))
			receipt, err := s.Receipt(ctx, owner, session, segment)
			if err != nil {
				return err
			}
			if receipt == nil {
				continue
			}
			receipt.Text = ""
			plain, _ := json.Marshal(receipt)
			data, nonce, err := s.Cipher.Encrypt(plain, []byte(key))
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(sealedReceipt{data, nonce})
			if err = s.Client.SetArgs(ctx, key, string(raw), redis.SetArgs{Mode: "XX", KeepTTL: true}).Err(); err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}
