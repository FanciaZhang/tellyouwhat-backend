package albums

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type retentionSendFunc func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error)

func (f retentionSendFunc) sendRetention(ctx context.Context, token string, c albumRetentionReminder, id string) (albumAPNSResult, error) {
	return f(ctx, token, c, id)
}

type retentionDispatchFixture struct {
	push          *pushFixture
	d             *albumRetentionDispatcher
	sessions      []AlbumSession
	registrations []albumPushRegistration
	proof         VerifiedAlbumSubscription
	receipt       albumReceiptFixture
	clock         time.Time
	owner         string
}

func newRetentionDispatchFixture(t *testing.T, devices int) *retentionDispatchFixture {
	t.Helper()
	p := newPushFixture(t)
	p.now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	f := &retentionDispatchFixture{push: p}
	for i := range devices {
		session, _ := p.login(t, "retention-owner", p.now.Add(time.Duration(i)*time.Microsecond))
		registered, err := p.r.update(context.Background(), session.Token, pushUpdate(fmt.Sprintf("%064x", i+1), 0), p.now)
		if err != nil {
			t.Fatal(err)
		}
		f.sessions = append(f.sessions, session)
		f.registrations = append(f.registrations, registered)
		f.owner = session.AccountID
	}
	if devices == 0 {
		s, _ := p.login(t, "retention-owner", p.now)
		f.owner = s.AccountID
	}
	r := NewMySQLUploads(p.r.db).WithSubscriptionEnvironment("Sandbox")
	if _, err := r.db.Exec(`UPDATE album_storage_accounts SET used_bytes=100 WHERE owner_id=?`, f.owner); err != nil {
		t.Fatal(err)
	}
	f.proof = VerifiedAlbumSubscription{"retention-chain", "retention-transaction", Cloud50GBAnnualProduct, "Sandbox", time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC), p.now, false}
	account := applyReminderSubscription(t, r, f.owner, f.proof, p.now)
	f.clock = account.RetentionDeadline.AddDate(0, 0, -30)
	f.receipt = newAlbumReceiptFixture(t, f.clock)
	client := reminderStatusFunc(func(context.Context, string) ([]appstore.SubscriptionStatus, error) {
		return []appstore.SubscriptionStatus{reminderSignedStatus(t, f.receipt, f.owner, f.proof)}, nil
	})
	service := reminderVerificationService(t, r, f.receipt, client, func() time.Time { return f.clock })
	f.d = &albumRetentionDispatcher{r, service, p.r, retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		return albumAPNSResult{Status: "accepted"}, nil
	}), func() time.Time { return f.clock }, nil}
	return f
}
func (f *retentionDispatchFixture) reminder(t *testing.T) albumRetentionReminder {
	t.Helper()
	c, err := scanReminder(f.d.repository.db.QueryRow(`SELECT `+reminderColumns+` FROM album_retention_reminders WHERE owner_id=? AND days_before=30`, f.owner))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestMySQLRetentionDispatchPartialRetryAndRestart(t *testing.T) {
	f := newRetentionDispatchFixture(t, 2)
	calls := map[string][]string{}
	retryAt := f.clock.Add(2 * time.Hour)
	f.d.sender = retentionSendFunc(func(_ context.Context, token string, c albumRetentionReminder, id string) (albumAPNSResult, error) {
		if id == c.ID || !validOwner(id) {
			t.Error("delivery must have its own stable request ID")
		}
		calls[token] = append(calls[token], id)
		if token == fmt.Sprintf("%064x", 2) && len(calls[token]) == 1 {
			return albumAPNSResult{Status: "retry", RetryAt: retryAt}, nil
		}
		return albumAPNSResult{Status: "accepted"}, nil
	})
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal(found, err)
	}
	first := f.reminder(t)
	if first.State != "pending" || !first.Available.Equal(retryAt) || first.Token != "" {
		t.Fatal("partial retry not persisted", first.State, first.Available)
	}
	f.clock = retryAt.Add(-time.Microsecond)
	if found, err := f.d.dispatchOne(context.Background()); err != nil || found {
		t.Fatal("retry-after violated", found, err)
	}
	// Reopen the repository/registration adapter while keeping only durable state.
	f.d.repository = NewMySQLUploads(f.push.r.db).WithSubscriptionEnvironment("Sandbox")
	f.d.registrations, _ = newAlbumPushRegistrations(f.push.r.db, f.push.r.cipher, "production")
	f.clock = retryAt
	if found, err := f.d.dispatchOne(context.Background()); err != nil || !found {
		t.Fatal("restart", found, err)
	}
	for token, ids := range calls {
		want := 1
		if token == fmt.Sprintf("%064x", 2) {
			want = 2
		}
		if len(ids) != want || ids[0] != ids[len(ids)-1] {
			t.Fatal("duplicate acceptance or unstable retry identity", len(ids), want)
		}
	}
	if len(calls) != 2 || f.reminder(t).State != "delivered" {
		t.Fatal("missing multi-device acceptance")
	}
	var count int
	if err := f.push.r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_deliveries WHERE state='accepted' AND accepted_at IS NOT NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatal("durable receipt", count, err)
	}
	account, _ := f.d.repository.CloudAccount(context.Background(), f.owner, f.clock)
	if account.UsedBytes != 100 {
		t.Fatal("dispatch modified media usage")
	}
}
func TestMySQLRetentionDispatchFencesChanges(t *testing.T) {
	for _, name := range []string{"logout", "permission-off", "account-disabled", "grant-revoked", "renewal", "provider-failure", "wrong-push-environment"} {
		t.Run(name, func(t *testing.T) {
			f := newRetentionDispatchFixture(t, 1)
			sends := 0
			f.d.sender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
				sends++
				return albumAPNSResult{Status: "accepted"}, nil
			})
			f.d.subscriptions.resolver.client = reminderStatusFunc(func(context.Context, string) ([]appstore.SubscriptionStatus, error) {
				switch name {
				case "logout":
					if err := f.push.sessions.Revoke(context.Background(), f.sessions[0].Token); err != nil {
						t.Fatal(err)
					}
				case "permission-off":
					if _, err := f.push.r.update(context.Background(), f.sessions[0].Token, pushUpdate("", f.registrations[0].Revision), f.push.now); err != nil {
						t.Fatal(err)
					}
				case "account-disabled":
					if _, err := f.push.r.db.Exec(`UPDATE album_accounts SET state='disabled' WHERE account_id=?`, f.owner); err != nil {
						t.Fatal(err)
					}
				case "grant-revoked":
					if _, err := f.push.r.db.Exec(`UPDATE album_apple_grants SET revoked_at=? WHERE account_id=?`, f.clock, f.owner); err != nil {
						t.Fatal(err)
					}
				case "renewal":
					v := f.proof
					v.Active = true
					v.ExpiresAt = f.clock.AddDate(1, 0, 0)
					v.SignedAt = f.clock
					v.TransactionID = "renewed"
					applyReminderSubscription(t, f.d.repository, f.owner, v, f.clock)
				case "provider-failure":
					return nil, errors.New("provider unavailable")
				}
				return []appstore.SubscriptionStatus{reminderSignedStatus(t, f.receipt, f.owner, f.proof)}, nil
			})
			if name == "wrong-push-environment" {
				f.d.registrations, _ = newAlbumPushRegistrations(f.push.r.db, f.push.r.cipher, "development")
			}
			found, err := f.d.dispatchOne(context.Background())
			if !found || sends != 0 {
				t.Fatal("old intent sent after change", found, sends, err)
			}
			if name == "provider-failure" && !errors.Is(err, ErrSubscriptionUnavailable) {
				t.Fatal("missing provider failure", err)
			}
			if name == "renewal" && f.reminder(t).State != "cancelled" {
				t.Fatal("renewal left old queue live")
			}
			if name != "renewal" && f.reminder(t).State != "pending" {
				t.Fatal("unsent notification falsely acknowledged")
			}
		})
	}
}
func TestMySQLRetentionDispatchAmbiguousAndRejected(t *testing.T) {
	for _, mode := range []string{"ambiguous", "rejected", "unregistered", "late-unregistered-after-rotation"} {
		t.Run(mode, func(t *testing.T) {
			f := newRetentionDispatchFixture(t, 1)
			calls := 0
			var firstID string
			f.d.sender = retentionSendFunc(func(_ context.Context, _ string, _ albumRetentionReminder, id string) (albumAPNSResult, error) {
				calls++
				if calls == 1 {
					firstID = id
				} else if mode == "ambiguous" && id != firstID {
					t.Error("ambiguous retry ID changed")
				}
				if mode == "ambiguous" {
					return albumAPNSResult{}, errAlbumAPNS
				}
				if mode == "late-unregistered-after-rotation" {
					if calls > 1 {
						return albumAPNSResult{Status: "accepted"}, nil
					}
					if _, err := f.push.r.update(context.Background(), f.sessions[0].Token, pushUpdate(strings.Repeat("ab", 32), 1), f.push.now.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					return albumAPNSResult{Status: "unregistered", InvalidatedAt: f.push.now}, nil
				}
				if mode == "unregistered" {
					return albumAPNSResult{Status: mode, InvalidatedAt: f.clock}, nil
				}
				return albumAPNSResult{Status: mode}, nil
			})
			_, err := f.d.dispatchOne(context.Background())
			if calls < 1 {
				t.Fatal("no attempt")
			}
			if mode == "ambiguous" {
				if !errors.Is(err, errAlbumAPNS) || !f.reminder(t).Available.Equal(f.clock.Add(15*time.Minute)) {
					t.Fatal("ambiguous outcome retry", err)
				}
				f.clock = f.clock.Add(15 * time.Minute)
				_, _ = f.d.dispatchOne(context.Background())
				if calls != 2 {
					t.Fatal("missing ambiguous retry")
				}
			} else if mode == "rejected" {
				f.clock = f.clock.Add(time.Hour)
				_, _ = f.d.dispatchOne(context.Background())
				if calls != 1 {
					t.Fatal("permanent rejection blindly retried")
				}
			} else if mode == "unregistered" {
				if _, err := f.push.r.target(context.Background(), f.owner, f.registrations[0].ID, 1); err == nil {
					t.Fatal("invalid device remained active")
				}
			} else {
				if _, err := f.push.r.target(context.Background(), f.owner, f.registrations[0].ID, 2); err != nil {
					t.Fatal("old 410 disabled fresh registration", err)
				}
			}
		})
	}
}
func TestMySQLRetentionDispatchConcurrentWorkersAndLateReceipt(t *testing.T) {
	f := newRetentionDispatchFixture(t, 1)
	var mu sync.Mutex
	calls := 0
	f.d.sender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return albumAPNSResult{Status: "accepted"}, nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.d.dispatchOne(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatal("concurrent duplicate", calls)
	}
	// A receipt for an older attempt cannot overwrite a new retry schedule.
	f = newRetentionDispatchFixture(t, 1)
	claim := takeReminder(t, f.d.repository, f.clock, 30)
	revision, err := f.d.subscriptions.verifyRetentionReminder(context.Background(), f.d.repository, claim)
	if err != nil {
		t.Fatal(err)
	}
	old, found, err := f.d.repository.nextRetentionDelivery(context.Background(), claim, revision, []string{"production"}, f.clock)
	if err != nil || !found {
		t.Fatal(err)
	}
	f.clock = claim.Until
	newer := takeReminder(t, f.d.repository, f.clock, 30)
	revision, err = f.d.subscriptions.verifyRetentionReminder(context.Background(), f.d.repository, newer)
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := f.d.repository.nextRetentionDelivery(context.Background(), newer, revision, []string{"production"}, f.clock)
	if err != nil || !found || current.ID != old.ID {
		t.Fatal("restart identity", err)
	}
	if err := f.d.repository.recordRetentionDelivery(context.Background(), claim, old, albumAPNSResult{Status: "accepted"}, f.clock); !errors.Is(err, ErrLease) {
		t.Fatal("stale receipt accepted", err)
	}
	if err := f.d.repository.recordRetentionDelivery(context.Background(), newer, current, albumAPNSResult{Status: "accepted"}, f.clock.Add(time.Minute)); err != nil {
		t.Fatal("late accepted receipt lost", err)
	}
}
func TestMySQLRetentionDispatchStopsBetweenDevicesOnRenewal(t *testing.T) {
	f := newRetentionDispatchFixture(t, 2)
	sends := 0
	f.d.sender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		sends++
		v := f.proof
		v.Active = true
		v.ExpiresAt = f.clock.AddDate(1, 0, 0)
		v.SignedAt = f.clock.Add(time.Millisecond)
		v.TransactionID = uuid.NewString()
		applyReminderSubscription(t, f.d.repository, f.owner, v, f.clock)
		return albumAPNSResult{Status: "accepted"}, nil
	})
	if _, err := f.d.dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sends != 1 || f.reminder(t).State != "cancelled" {
		t.Fatal("renewal did not stop remaining devices", sends)
	}
}

func TestMySQLRetentionDispatchRuntimeUsesAPNSAdapter(t *testing.T) {
	f := newRetentionDispatchFixture(t, 1)
	config, _, _, _ := apnsFixture(t)
	requests := make(chan string, 1)
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.push.apple.com" || r.Header.Get("apns-topic") != albumDevelopmentAudience || r.Header.Get("authorization") == "" {
			t.Error("incorrect runtime APNs wiring")
		}
		id := r.Header.Get("apns-id")
		if !validOwner(id) {
			t.Error("missing stable delivery ID")
		}
		requests <- id
		return &http.Response{StatusCode: 200, ProtoMajor: 2, Header: http.Header{"Apns-Id": []string{id}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	sender, err := newAlbumAPNS(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	sender.now = func() time.Time { return f.clock }
	f.d.sender = sender
	gateway := &Gateway{}
	gateway.startRetentionDispatch(context.Background(), f.d)
	t.Cleanup(func() { gateway.BeginDrain(); gateway.subscriptionWorkers.Wait() })
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime never sent eligible reminder")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var count int
		if err := f.push.r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=? AND state='delivered'`, f.owner).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("APNs acceptance not persisted")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestMySQLRetentionDispatchShutdownPreservesKnownAcceptance(t *testing.T) {
	f := newRetentionDispatchFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.d.sender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		cancel()
		return albumAPNSResult{Status: "accepted"}, nil
	})
	if found, _ := f.d.dispatchOne(ctx); !found {
		t.Fatal("no dispatch")
	}
	var count int
	if err := f.push.r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_deliveries WHERE state='accepted'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("shutdown lost known acceptance", count, err)
	}
	// After the lease expires, restarting finishes the parent without a new push.
	f.clock = f.clock.Add(time.Minute)
	f.d.sender = retentionSendFunc(func(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error) {
		t.Error("accepted device resent after shutdown")
		return albumAPNSResult{}, errAlbumAPNS
	})
	if _, err := f.d.dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reminder(t).State != "delivered" {
		t.Fatal("restart did not finish parent")
	}
}
