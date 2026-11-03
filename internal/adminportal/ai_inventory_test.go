package adminportal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/arkcontrol"
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

func TestModelPricesRemainAvailableWhenWholeAccountInventoryFails(t *testing.T) {
	config := &AIConfig{}
	config.inventoryCache().activations.get(context.Background(), func(context.Context) ([]arkcontrol.Activation, error) {
		return nil, errors.New("whole account timed out")
	})
	calls := 0
	read := func(_ context.Context, names []string) ([]arkcontrol.Activation, error) {
		calls++
		if len(names) != 1 {
			t.Fatal("unbounded account price query")
		}
		return []arkcontrol.Activation{{Name: names[0], State: "Available"}}, nil
	}
	first := config.modelPriceSnapshot(context.Background(), "model-one", read)
	again := config.modelPriceSnapshot(context.Background(), "model-one", read)
	second := config.modelPriceSnapshot(context.Background(), "model-two", read)
	if calls != 2 || first.Stale || first.SyncedAt == nil || again.Value[0].Name != "model-one" || second.Value[0].Name != "model-two" {
		t.Fatal("selected prices depend on whole account cache or cross model boundary")
	}
	entry := config.inventoryCache().modelPrices["model-one"]
	entry.next = time.Time{}
	stale := config.modelPriceSnapshot(context.Background(), "model-one", func(context.Context, []string) ([]arkcontrol.Activation, error) {
		return nil, errors.New("private upstream message")
	})
	if !stale.Stale || stale.Value[0].Name != "model-one" || stale.SyncError == "private upstream message" {
		t.Fatal("last known selected model price was lost")
	}
}
