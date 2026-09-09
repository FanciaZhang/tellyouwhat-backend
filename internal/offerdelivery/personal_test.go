package offerdelivery

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
)

type personalCloudFixture struct {
	mu          sync.Mutex
	createCount int
	pools       []appstoreconnect.CodePool
	uncertain   bool
	rejection   error
}

func (f *personalCloudFixture) ListOffers(context.Context) ([]appstoreconnect.Offer, error) {
	return []appstoreconnect.Offer{{ID: "friends", Name: "FRIENDS", Active: true, SubscriptionID: "456", ProductID: "app.monthly"}}, nil
}
func (f *personalCloudFixture) ListCodePools(context.Context, string) ([]appstoreconnect.CodePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pools, nil
}
func (f *personalCloudFixture) CreateCustomCode(_ context.Context, offer, code string, count int, expiry string) (appstoreconnect.CodePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount++
	if f.rejection != nil {
		return appstoreconnect.CodePool{}, f.rejection
	}
	if offer != "friends" || count != 1 {
		return appstoreconnect.CodePool{}, ErrInvalid
	}
	p := appstoreconnect.CodePool{ID: "personal-cloud-pool", Code: code, Kind: "custom", NumberOfCodes: count, ExpirationDate: expiry, Active: true}
	f.pools = append(f.pools, p)
	if f.uncertain {
		return appstoreconnect.CodePool{}, appstoreconnect.ErrUnavailable
	}
	return p, nil
}
func TestPersonalCodeResumesUncertainCreationWithoutDuplication(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	id := uuid.NewString()
	expiry := now.AddDate(0, 0, 30).Format("2006-01-02")
	v, err := s.PreparePersonal(ctx, "health", "friends", id, "Known friend", expiry, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v.Code, "Known") || len(v.Code) < 30 {
		t.Fatal("personal code is identifiable or guessable")
	}
	var encrypted []byte
	if err = s.DB.QueryRow(`SELECT ciphertext FROM offer_personal_deliveries WHERE app_id='health' AND id=?`, id).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte("Known friend")) || bytes.Contains(encrypted, []byte(v.Code)) {
		t.Fatal("intent exposed personal details", err)
	}
	cloud := &personalCloudFixture{uncertain: true}
	if _, _, err = s.CompletePersonal(ctx, "health", id, "admin", cloud, now); !errors.Is(err, ErrPersonalUncertain) {
		t.Fatal(err)
	}
	v, r, err := s.CompletePersonal(ctx, "health", id, "admin", cloud, now)
	if err != nil || v.State != "ready" || r.Status != "assigned" || r.ClaimExpiresAt == nil || cloud.createCount != 1 {
		t.Fatalf("resume failed %+v %+v %v", v, r, err)
	}
	if _, _, err = s.CompletePersonal(ctx, "health", id, "admin", cloud, now); err != nil || cloud.createCount != 1 {
		t.Fatal("retry created another cloud code", err)
	}
	raw := []byte(reportHeader + reportLine(now.AddDate(0, 0, -1).Format("2006-01-02"), "123", "456", "FRIENDS", v.Code, 1))
	if err = s.ImportAppleReport(ctx, "health", "123", now.AddDate(0, 0, -1).Format("2006-01-02"), raw, now); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPool(ctx, "health", v.PoolID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.PoolReport(ctx, "health", p)
	if err != nil || report.Redemptions != 1 {
		t.Fatalf("dedicated code redemption not found %+v %v", report, err)
	}
	r, err = s.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, false, now)
	if err != nil {
		t.Fatal(err)
	}
	_, retry, err := s.CompletePersonal(ctx, "health", id, "admin", cloud, now)
	if err != nil || retry.ClaimExpiresAt != nil {
		t.Fatal("idempotent create revived a revoked link", err)
	}
	if _, err = s.PreparePersonal(ctx, "health", "friends", id, "Different friend", expiry, now); !errors.Is(err, ErrConflict) {
		t.Fatal("request reused for another recipient")
	}
	if _, _, err = s.CompletePersonal(ctx, "journal", id, "admin", cloud, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-App intent access")
	}
}
func TestPersonalConcurrentCompletionAllocatesOnce(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := s.PreparePersonal(ctx, "health", "friends", id, "Friend", now.AddDate(0, 0, 30).Format("2006-01-02"), now); err != nil {
		t.Fatal(err)
	}
	cloud := &personalCloudFixture{}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := s.CompletePersonal(ctx, "health", id, "admin", cloud, now)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if succeeded < 1 || cloud.createCount != 1 {
		t.Fatalf("concurrent duplicate: success=%d creates=%d", succeeded, cloud.createCount)
	}
}
func TestPersonalDefinitiveAppleRejectionDoesNotRaiseCapacity(t *testing.T) {
	s, _, now := fixture(t)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := s.PreparePersonal(ctx, "health", "friends", id, "Friend", now.AddDate(0, 0, 30).Format("2006-01-02"), now); err != nil {
		t.Fatal(err)
	}
	cloud := &personalCloudFixture{rejection: appstoreconnect.ErrRejected}
	for range 2 {
		if _, _, err := s.CompletePersonal(ctx, "health", id, "admin", cloud, now); !errors.Is(err, appstoreconnect.ErrRejected) {
			t.Fatal(err)
		}
	}
	if cloud.createCount != 1 {
		t.Fatal("retried a rejected code or changed capacity")
	}
}
func TestAppleCodeExpiryUsesPacificMidnight(t *testing.T) {
	for _, v := range []struct{ day, hour string }{{"2026-09-09", "07:00"}, {"2026-12-09", "08:00"}} {
		got, err := AppleCodeExpiry(v.day)
		if err != nil || got.Format("15:04") != v.hour {
			t.Fatalf("expiry=%v err=%v", got, err)
		}
	}
}
