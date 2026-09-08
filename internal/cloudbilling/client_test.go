package cloudbilling

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/billing"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/volcengineerr"
)

type fakeAPI func(*billing.ListBillOverviewByProdInput) (*billing.ListBillOverviewByProdOutput, error)

func (f fakeAPI) ListBillOverviewByProdWithContext(_ volcengine.Context, in *billing.ListBillOverviewByProdInput, _ ...request.Option) (*billing.ListBillOverviewByProdOutput, error) {
	return f(in)
}
func TestBillsRequireCompletePagesAndExactMoney(t *testing.T) {
	row := &billing.ListForListBillOverviewByProdOutput{Product: volcengine.String("ark_bd"), Currency: volcengine.String("CNY"), PayableAmount: volcengine.String("7.85"), PaidAmount: volcengine.String("0.000000001")}
	calls := 0
	client := Client{API: fakeAPI(func(in *billing.ListBillOverviewByProdInput) (*billing.ListBillOverviewByProdOutput, error) {
		calls++
		if *in.Offset != int32(calls-1) || *in.BillPeriod != "2026-09" {
			t.Fatal("pagination not advanced")
		}
		return &billing.ListBillOverviewByProdOutput{Total: volcengine.Int32(2), List: []*billing.ListForListBillOverviewByProdOutput{row}}, nil
	})}
	out, err := client.Read(context.Background(), "2026-09")
	if err != nil || len(out) != 2 || calls != 2 || out[0].PayableNanos != 7850000000 || out[0].PaidNanos != 1 {
		t.Fatalf("bill: %+v %v calls=%d", out, err, calls)
	}
	client.API = fakeAPI(func(in *billing.ListBillOverviewByProdInput) (*billing.ListBillOverviewByProdOutput, error) {
		return &billing.ListBillOverviewByProdOutput{Total: volcengine.Int32(2), List: []*billing.ListForListBillOverviewByProdOutput{}}, nil
	})
	if _, err = client.Read(context.Background(), "2026-09"); !errors.Is(err, ErrResponse) {
		t.Fatal("truncated pagination accepted")
	}
	for _, raw := range []string{"", "NaN", "1e9", "0.0000000001", "999999999999999999"} {
		if _, err = parseAmount(&raw); err == nil {
			t.Fatalf("invalid money accepted: %s", raw)
		}
	}
	client.API = fakeAPI(func(in *billing.ListBillOverviewByProdInput) (*billing.ListBillOverviewByProdOutput, error) {
		return nil, volcengineerr.NewRequestFailure(volcengineerr.New("AccessDenied", "hidden message", nil), 200, "hidden request")
	})
	if _, err = client.Read(context.Background(), "2026-09"); err == nil {
		t.Fatal("HTTP 200 provider error accepted")
	}
}

type fakeReader func(context.Context, string) ([]Product, error)

func (f fakeReader) Read(ctx context.Context, p string) ([]Product, error) { return f(ctx, p) }
func TestCacheFailureKeepsSourceTimeAndNewMonthDropsOldAmounts(t *testing.T) {
	now := time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	c := Cache{Reader: fakeReader(func(context.Context, string) ([]Product, error) { return nil, errors.New("unavailable") }), value: Snapshot{Status: "ready", Period: "2026-09", FetchedAt: &old, Products: []Product{{PayableNanos: 1}}}}
	c.refresh("2026-09", now)
	if c.value.Status != "stale" || !c.value.FetchedAt.Equal(old) || len(c.value.Products) != 1 {
		t.Fatal("failed refresh fabricated current data")
	}
	c.Reader = fakeReader(func(context.Context, string) ([]Product, error) { return []Product{}, nil })
	value := c.Snapshot(now)
	if value.Period != "2026-10" || value.FetchedAt != nil || len(value.Products) != 0 {
		t.Fatalf("old bill leaked across month: %+v", value)
	}
}

func TestMonthChangeWhileOldBillIsStillLoading(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := Cache{Reader: fakeReader(func(context.Context, string) ([]Product, error) {
		close(started)
		<-release
		return []Product{{PayableNanos: 123}}, nil
	}), loading: true, value: Snapshot{Status: "ready", Period: "2026-09", Products: []Product{{PayableNanos: 100}}}}
	go func() { c.refresh("2026-09", time.Now()); close(done) }()
	<-started
	value := c.Snapshot(time.Date(2026, 9, 30, 17, 0, 0, 0, time.UTC))
	if value.Period != "2026-10" || len(value.Products) != 0 {
		t.Fatal("old month visible while loading")
	}
	close(release)
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value.Period != "2026-10" || len(c.value.Products) != 0 || c.value.FetchedAt != nil {
		t.Fatal("old request overwrote current month")
	}
}
