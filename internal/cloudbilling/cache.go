package cloudbilling

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/volcengine/volcengine-go-sdk/volcengine/volcengineerr"
)

type Reader interface {
	Read(context.Context, string) ([]Product, error)
}
type Snapshot struct {
	Status      string     `json:"status"`
	Period      string     `json:"period"`
	FetchedAt   *time.Time `json:"fetchedAt"`
	AttemptedAt *time.Time `json:"attemptedAt"`
	Products    []Product  `json:"products"`
}
type Cache struct {
	Reader  Reader
	mu      sync.Mutex
	value   Snapshot
	next    time.Time
	loading bool
}

// Snapshot never blocks local operations metrics on the supplier. A failed refresh
// keeps its original retrieval timestamp and is explicitly marked stale.
func (c *Cache) Snapshot(now time.Time) Snapshot {
	if c == nil || c.Reader == nil {
		return Snapshot{Status: "not_configured", Products: []Product{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	period := now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01")
	if c.value.Period != period && !c.loading {
		c.value = Snapshot{Status: "loading", Period: period, Products: []Product{}}
		c.next = time.Time{}
	}
	if !c.loading && !now.Before(c.next) {
		c.loading = true
		go c.refresh(period, now)
	}
	result := c.value
	result.Products = append([]Product{}, result.Products...)
	return result
}
func (c *Cache) refresh(period string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	products, err := c.Reader.Read(ctx, period)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loading = false
	c.value.AttemptedAt = &now
	if err == nil {
		fetched := time.Now().UTC()
		c.value = Snapshot{Status: "ready", Period: period, FetchedAt: &fetched, AttemptedAt: &now, Products: products}
		c.next = now.Add(time.Hour)
		return
	}
	c.next = now.Add(time.Minute)
	c.value.Status = "unavailable"
	var failure volcengineerr.Error
	if errors.As(err, &failure) && (failure.Code() == "AccessDenied" || failure.Code() == "Unauthorized") {
		c.value.Status = "access_denied"
	}
	if c.value.FetchedAt != nil {
		c.value.Status = "stale"
	}
}
