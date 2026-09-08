package promptconfig

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureLoader struct {
	calls   atomic.Int32
	fail    atomic.Bool
	gate    chan struct{}
	version atomic.Int32
}

func (l *fixtureLoader) Published(ctx context.Context) (map[string]Revision, error) {
	l.calls.Add(1)
	if l.gate != nil {
		select {
		case <-l.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if l.fail.Load() {
		return nil, errors.New("offline")
	}
	now := time.Now()
	out := map[string]Revision{}
	for scope, p := range Defaults("lite", "pro", "voice", 90) {
		out[scope] = Revision{ID: now.String(), Scope: scope, Policy: p, PublishedAt: &now}
	}
	return out, nil
}
func TestCacheCoalescesRefreshRetainsSnapshotAndFreezesRequests(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	loader := &fixtureLoader{gate: make(chan struct{})}
	c := NewCache(loader)
	if _, err := c.Current("journal"); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Refresh(ctx, now); err != nil {
				t.Error(err)
			}
		}()
	}
	for loader.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(loader.gate)
	wg.Wait()
	if loader.calls.Load() != 1 {
		t.Fatal("concurrent refresh did not coalesce", loader.calls.Load())
	}
	frozen, err := Freeze(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := FromContext(frozen)
	r, _ := c.Current("journal")
	r.Policy.Journal.Styles[0].Prompt = "corrupted"
	again, _ := c.Current("journal")
	if again.Policy.Journal.Styles[0].Prompt == "corrupted" {
		t.Fatal("mutable snapshot escaped")
	}
	if err := c.Refresh(ctx, now.Add(RefreshInterval-time.Nanosecond)); err != nil || loader.calls.Load() != 1 {
		t.Fatal("queried before interval", err)
	}
	loader.fail.Store(true)
	if c.Refresh(ctx, now.Add(RefreshInterval)) == nil {
		t.Fatal("failure hidden")
	}
	retained, _ := c.Current("journal")
	if retained.ID != before.ID {
		t.Fatal("failure discarded snapshot")
	}
	loader.fail.Store(false)
	if err := c.Refresh(ctx, now.Add(2*RefreshInterval)); err != nil {
		t.Fatal(err)
	}
	fresh, _ := c.Current("journal")
	old, _ := FromContext(frozen)
	if old.ID != before.ID || fresh.ID == old.ID {
		t.Fatal("request version changed")
	}
	if _, _, err := c.Status(); err != nil {
		t.Fatal(err)
	}
}
func TestStartupFailureNeverPublishesPartialSnapshot(t *testing.T) {
	loader := &fixtureLoader{}
	loader.fail.Store(true)
	c := NewCache(loader)
	if c.Refresh(context.Background(), time.Now()) == nil {
		t.Fatal("startup failure hidden")
	}
	if _, err := c.Current("journal"); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}
