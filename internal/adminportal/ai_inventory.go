package adminportal

import (
	"context"
	"sync"
	"time"

	"github.com/tellyouwhat/backend/internal/arkcontrol"
)

type inventorySnapshot[T any] struct {
	Value     T          `json:"value"`
	SyncedAt  *time.Time `json:"syncedAt"`
	Stale     bool       `json:"stale"`
	SyncError string     `json:"syncError,omitempty"`
}
type inventoryEntry[T any] struct {
	mu       sync.Mutex
	next     time.Time
	snapshot inventorySnapshot[T]
}

func (e *inventoryEntry[T]) get(ctx context.Context, read func(context.Context) (T, error)) inventorySnapshot[T] {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Now().Before(e.next) {
		return e.snapshot
	}
	value, err := read(ctx)
	if err != nil {
		e.snapshot.Stale = true
		e.snapshot.SyncError = "火山同步失败；显示最近成功的数据"
		e.next = time.Now().Add(5 * time.Second)
		return e.snapshot
	}
	now := time.Now().UTC()
	e.snapshot = inventorySnapshot[T]{Value: value, SyncedAt: &now}
	e.next = now.Add(30 * time.Second)
	return e.snapshot
}

type aiInventoryCache struct {
	mu          sync.Mutex
	endpoints   map[string]*inventoryEntry[arkcontrol.Endpoint]
	models      inventoryEntry[[]arkcontrol.Model]
	activations inventoryEntry[[]arkcontrol.Activation]
	modelPrices map[string]*inventoryEntry[[]arkcontrol.Activation]
}

func (a *AIConfig) inventoryCache() *aiInventoryCache {
	a.cacheOnce.Do(func() {
		a.cache = &aiInventoryCache{endpoints: make(map[string]*inventoryEntry[arkcontrol.Endpoint]), modelPrices: make(map[string]*inventoryEntry[[]arkcontrol.Activation])}
	})
	return a.cache
}

func (a *AIConfig) modelPriceSnapshot(ctx context.Context, model string, read func(context.Context, []string) ([]arkcontrol.Activation, error)) inventorySnapshot[[]arkcontrol.Activation] {
	cache := a.inventoryCache()
	cache.mu.Lock()
	entry := cache.modelPrices[model]
	if entry == nil {
		entry = &inventoryEntry[[]arkcontrol.Activation]{}
		cache.modelPrices[model] = entry
	}
	cache.mu.Unlock()
	return entry.get(ctx, func(ctx context.Context) ([]arkcontrol.Activation, error) { return read(ctx, []string{model}) })
}
func (a *AIConfig) endpointSnapshot(ctx context.Context, id string) inventorySnapshot[arkcontrol.Endpoint] {
	cache := a.inventoryCache()
	cache.mu.Lock()
	entry := cache.endpoints[id]
	if entry == nil {
		entry = &inventoryEntry[arkcontrol.Endpoint]{}
		cache.endpoints[id] = entry
	}
	cache.mu.Unlock()
	return entry.get(ctx, func(ctx context.Context) (arkcontrol.Endpoint, error) { return a.Inventory.Endpoint(ctx, id) })
}
