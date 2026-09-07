package adminportal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestInventoryCacheCoalescesAndRetainsLastSuccess(t *testing.T) {
	entry := inventoryEntry[string]{}
	calls := 0
	read := func(context.Context) (string, error) { calls++; return "baseline", nil }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); entry.get(context.Background(), read) }()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("concurrent snapshot fetched %d times", calls)
	}
	first := entry.snapshot
	entry.next = time.Time{}
	stale := entry.get(context.Background(), func(context.Context) (string, error) { return "", errors.New("secret diagnostic") })
	if !stale.Stale || stale.Value != "baseline" || !stale.SyncedAt.Equal(*first.SyncedAt) || stale.SyncError == "secret diagnostic" {
		t.Fatal("failed refresh changed successful snapshot")
	}
	empty := inventoryEntry[string]{}
	missing := empty.get(context.Background(), func(context.Context) (string, error) { return "", errors.New("offline") })
	if missing.SyncedAt != nil || !missing.Stale {
		t.Fatal("failed read was presented as synchronized")
	}
}
