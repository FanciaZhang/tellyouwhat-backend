package promptconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

const RefreshInterval = 60 * time.Second

type Loader interface {
	Published(context.Context) (map[string]Revision, error)
}

func (s Store) Published(ctx context.Context) (map[string]Revision, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.document,r.published_at FROM prompt_config_current c JOIN prompt_config_revisions r ON r.id=c.revision_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Revision{}
	for rows.Next() {
		var raw []byte
		var at sql.NullTime
		var r Revision
		if err = rows.Scan(&raw, &at); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if at.Valid {
			r.PublishedAt = &at.Time
		}
		out[r.Scope] = r
	}
	return out, rows.Err()
}

type cacheSnapshot struct{ revisions map[string]Revision }
type Cache struct {
	loader      Loader
	snapshot    atomic.Pointer[cacheSnapshot]
	mu          sync.Mutex
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   error
	inFlight    chan struct{}
}

func NewCache(loader Loader) *Cache { return &Cache{loader: loader} }

// Current only reads memory. Return a private copy so callers cannot mutate the snapshot.
func (c *Cache) Current(scope string) (Revision, error) {
	if c == nil {
		return Revision{}, ErrNotReady
	}
	s := c.snapshot.Load()
	if s == nil {
		return Revision{}, ErrNotReady
	}
	r, ok := s.revisions[scope]
	if !ok {
		return Revision{}, ErrNotFound
	}
	return clone(r), nil
}
func (c *Cache) Refresh(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	if done := c.inFlight; done != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			c.mu.Lock()
			err := c.lastError
			c.mu.Unlock()
			return err
		}
	}
	if !c.lastAttempt.IsZero() && now.Sub(c.lastAttempt) < RefreshInterval {
		err := c.lastError
		c.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	c.inFlight = done
	c.lastAttempt = now
	c.mu.Unlock()
	values, err := c.loader.Published(ctx)
	if err == nil {
		for _, scope := range Scopes() {
			r, ok := values[scope]
			if !ok || r.ID == "" || r.Scope != scope || r.PublishedAt == nil || r.Policy.Validate(scope) != nil {
				err = ErrInvalid
				break
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.snapshot.Store(&cacheSnapshot{revisions: clone(values)})
		c.lastSuccess = now
	}
	c.lastError = err
	c.inFlight = nil
	close(done)
	return err
}
func (c *Cache) Status() (time.Time, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAttempt, c.lastSuccess, c.lastError
}
func (c *Cache) Run(ctx context.Context, report func(error)) {
	timer := time.NewTicker(RefreshInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			refresh, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Refresh(refresh, now)
			cancel()
			if err != nil && report != nil {
				report(err)
			}
		}
	}
}

// Start rejects an incomplete startup snapshot. A later refresh failure retains the last valid revision.
func Start(ctx context.Context, store Store, defaults map[string]Policy, report func(error)) (*Cache, error) {
	for _, scope := range Scopes() {
		p, ok := defaults[scope]
		if !ok {
			return nil, ErrInvalid
		}
		if err := store.Initialize(ctx, scope, p, time.Now()); err != nil {
			return nil, err
		}
	}
	c := NewCache(store)
	if err := c.Refresh(ctx, time.Now()); err != nil {
		return nil, err
	}
	go c.Run(ctx, report)
	return c, nil
}
