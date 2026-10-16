package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/migrations"
)

func (f albumReceiptFixture) notification(t *testing.T, id, kind string, transaction map[string]any) string {
	t.Helper()
	data := map[string]any{"bundleId": albumDevelopmentAudience, "environment": "Sandbox"}
	if transaction != nil {
		data["status"] = 1
		data["signedTransactionInfo"] = f.sign(t, transaction)
	}
	return f.sign(t, map[string]any{"notificationType": kind, "notificationUUID": id, "version": "2.0", "signedDate": f.now.UnixMilli(), "data": data})
}

func TestGatewayAlbumSubscriptionNotificationsCommitBeforeAckAndRejectForgery(t *testing.T) {
	db, dsn := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	values := gatewayEnvironment(t)
	values["ALBUM_DATABASE_DSN"] = dsn
	values["ALBUM_REDIS_URL"] = os.Getenv("REDIS_TEST_URL")
	config, err := LoadGatewayConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	config.Subscriptions = &AlbumSubscriptionConfig{BundleID: albumDevelopmentAudience, Environment: "Sandbox", IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: fixture.roots}
	owner := uuid.NewString()
	repository := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox")
	if err := repository.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	var state atomic.Value
	var unavailable atomic.Bool
	var calls atomic.Int32
	setCurrent := func(payload map[string]any, status int) {
		data, _ := json.Marshal(map[string]any{"bundleId": albumDevelopmentAudience, "environment": "Sandbox", "data": []any{map[string]any{"lastTransactions": []any{map[string]any{"status": status, "signedTransactionInfo": fixture.sign(t, payload)}}}}})
		state.Store(string(data))
	}
	transport := appleTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			t.Error("provider request not HTTPS")
		}
		if r.URL.Host == config.Worker.COS.Bucket+".cos."+config.Worker.COS.Region+".myqcloud.com" && r.URL.Query().Has("versioning") {
			return cosResponse(r, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, ""), nil
		}
		if r.URL.Host == "api.storekit-sandbox.apple.com" && r.URL.Path == "/inApps/v1/subscriptions/1000000000000002" && r.URL.RawQuery == "" {
			calls.Add(1)
			if unavailable.Load() {
				return nil, errors.New("private-provider-details")
			}
			body, _ := state.Load().(string)
			return appleResponse(body), nil
		}
		t.Error("unexpected provider operation")
		return nil, ErrSubscriptionUnavailable
	})
	active := fixture.payload(owner)
	setCurrent(active, 1)
	gateway, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)
	send := func(body []byte, method, host string) (int, []byte) {
		request, err := http.NewRequest(method, server.URL+"/v1/albums/subscription/notifications", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = host
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("private-provider-details")) {
			t.Fatal("private provider error escaped")
		}
		return response.StatusCode, data
	}
	encoded := func(signed string) []byte {
		data, _ := json.Marshal(map[string]string{"signedPayload": signed})
		return data
	}
	id := uuid.NewString()
	event := fixture.notification(t, id, "SUBSCRIBED", active)
	if status, _ := send(encoded(event), "POST", config.APIHost); status != 200 {
		t.Fatal("notification needed a user session or failed", status)
	}
	a, err := repository.CloudAccount(ctx, owner, time.Now())
	if err != nil || a.QuotaBytes != 50_000_000_000 || a.Revision != 1 {
		t.Fatal("ack before durable capacity", a, err)
	}
	// Re-signing identical semantics does not change its replay identity.
	if status, _ := send(encoded(fixture.notification(t, strings.ToUpper(id), "SUBSCRIBED", active)), "POST", config.APIHost); status != 200 || calls.Load() != 1 {
		t.Fatal("identical delivery reprocessed", status, calls.Load())
	}
	var deliveries sync.WaitGroup
	for range 8 {
		deliveries.Add(1)
		go func() {
			defer deliveries.Done()
			if status, _ := send(encoded(event), "POST", config.APIHost); status != 200 {
				t.Errorf("concurrent replay: %d", status)
			}
		}()
	}
	deliveries.Wait()
	if got, err := repository.CloudAccount(ctx, owner, time.Now()); err != nil || got.Revision != 1 || calls.Load() != 1 {
		t.Fatal("concurrent replay changed capacity", got, err, calls.Load())
	}
	for _, data := range []map[string]any{
		{"bundleId": "cn.tellyouwhat.health", "environment": "Sandbox", "signedTransactionInfo": fixture.sign(t, active)},
		{"bundleId": albumDevelopmentAudience, "environment": "Production", "signedTransactionInfo": fixture.sign(t, active)},
	} {
		foreign := fixture.sign(t, map[string]any{"notificationType": "SUBSCRIBED", "notificationUUID": uuid.NewString(), "version": "2.0", "signedDate": now.UnixMilli(), "data": data})
		if status, _ := send(encoded(foreign), "POST", config.APIHost); status != 422 || calls.Load() != 1 {
			t.Fatal("foreign audience or environment accepted", status, calls.Load())
		}
	}
	refund := fixture.payload(owner)
	refund["revocationDate"] = now.Add(-time.Minute).UnixMilli()
	refund["signedDate"] = now.Add(time.Second).UnixMilli()
	setCurrent(refund, 5)
	refundID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=123 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	if status, _ := send(encoded(fixture.notification(t, refundID, "REFUND", refund)), "POST", config.APIHost); status != 200 {
		t.Fatal("refund notification", status)
	}
	a, err = repository.CloudAccount(ctx, owner, time.Now())
	if err != nil || a.QuotaBytes != 0 || a.State != "expired" || a.UsedBytes != 123 || a.RetentionDeadline == nil {
		t.Fatal("refund did not retain existing bytes", a, err)
	}
	var reminders int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=? AND state='pending' AND retention_deadline=?`, owner, a.RetentionDeadline).Scan(&reminders); err != nil || reminders != 3 {
		t.Fatal("notification acknowledged before reminder schedule", reminders, err)
	}
	// A delayed renewal event still resolves the current revoked state.
	if status, _ := send(encoded(fixture.notification(t, uuid.NewString(), "DID_RENEW", active)), "POST", config.APIHost); status != 200 {
		t.Fatal("delayed event", status)
	}
	a, err = repository.CloudAccount(ctx, owner, time.Now())
	if err != nil || a.QuotaBytes != 0 {
		t.Fatal("old event resurrected capacity", err)
	}
	beforeCalls := calls.Load()
	parts := strings.Split(event, ".")
	parts[1] = "e30"
	if status, _ := send(encoded(strings.Join(parts, ".")), "POST", config.APIHost); status != 422 || calls.Load() != beforeCalls {
		t.Fatal("tampered event contacted provider", status)
	}
	if status, _ := send([]byte(`{"signedPayload":"unused","accountID":"forged"}`), "POST", config.APIHost); status != 400 {
		t.Fatal("owner injection accepted", status)
	}
	if status, _ := send(encoded(event), "POST", "health.example"); status != 421 {
		t.Fatal("foreign host accepted", status)
	}
	if status, _ := send(nil, "GET", config.APIHost); status != 401 {
		t.Fatal("public bypass exceeded exact POST route", status)
	}
	// TEST records acknowledge the verified diagnostic event without capacity.
	if status, _ := send(encoded(fixture.notification(t, uuid.NewString(), "TEST", nil)), "POST", config.APIHost); status != 200 || calls.Load() != beforeCalls {
		t.Fatal("test notification granted or queried capacity", status)
	}
	failedID := uuid.NewString()
	unavailable.Store(true)
	failed := encoded(fixture.notification(t, failedID, "DID_RENEW", active))
	if status, _ := send(failed, "POST", config.APIHost); status != 503 {
		t.Fatal("unavailable current state was acknowledged", status)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_subscription_notifications WHERE notification_id=?`, failedID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed event marked processed", err)
	}
	unavailable.Store(false)
	if status, _ := send(failed, "POST", config.APIHost); status != 200 {
		t.Fatal("retry did not recover", status)
	}
	var revision int64
	if err := db.QueryRowContext(ctx, `SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	// The same UUID with different verified semantics cannot collide.
	if status, _ := send(encoded(fixture.notification(t, refundID, "DID_RENEW", active)), "POST", config.APIHost); status != 422 {
		t.Fatal("notification identity collision accepted", status)
	}
	if got, err := repository.CloudAccount(ctx, owner, time.Now()); err != nil || got.Revision != revision || got.UsedBytes != 123 {
		t.Fatal("collision changed durable account", got, err)
	}
	gateway.Close()
	if status, _ := send(encoded(event), "POST", config.APIHost); status != 503 {
		t.Fatal("drained gateway accepted notification", status)
	}
	// A new, fully assembled Gateway picks up the persisted anchor without any
	// user request, client login or notification. Its own DB/Redis connections
	// and actual background worker execute this refresh.
	if _, err := db.ExecContext(ctx, `UPDATE album_cloud_subscriptions SET next_check_at=? WHERE owner_id=?`, time.Now().UTC().Add(-time.Second), owner); err != nil {
		t.Fatal(err)
	}
	beforeRestartCalls := calls.Load()
	restarted, err := NewGateway(ctx, config, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := repository.CloudAccount(ctx, owner, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision > revision {
			if got.Revision != revision+1 || calls.Load() != beforeRestartCalls+1 || got.QuotaBytes != 0 || got.UsedBytes != 123 {
				t.Fatal("restart refresh duplicated or changed retained bytes", got, calls.Load())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("assembled background workers did not resume persisted state")
		}
		time.Sleep(20 * time.Millisecond)
	}
	restarted.Close()
}

func TestMySQLAlbumSubscriptionRefreshClockExpiryKeepsSameSignedTransaction(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	owner := uuid.NewString()
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	signed := fixture.sign(t, fixture.payload(owner))
	client := &albumStatusStub{states: []appstore.SubscriptionStatus{{Status: 1, SignedTransaction: signed}}}
	resolver, _ := NewAlbumSubscriptionResolver(fixture.verifier(), client, "Sandbox", func() time.Time { return now })
	s, _ := NewAlbumSubscriptionService(r, resolver, func() time.Time { return now })
	active, err := s.Sync(ctx, owner, signed)
	if err != nil {
		t.Fatal(err)
	}
	staleActive, err := resolver.Resolve(ctx, owner, signed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=64 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	now = active.ExpiresAt.Add(time.Second)
	client.states = []appstore.SubscriptionStatus{{Status: 2, SignedTransaction: signed}}
	if found, err := s.refreshOne(ctx, r); !found || err != nil {
		t.Fatal("unchanged signed transaction did not expire", found, err)
	}
	expired, err := r.CloudAccount(ctx, owner, now)
	if err != nil || expired.State != "expired" || expired.QuotaBytes != 0 || expired.Revision != active.Revision+1 || expired.UsedBytes != 64 || !expired.ExpiresAt.Equal(*active.ExpiresAt) || expired.VerificationExpiresAt != nil {
		t.Fatal("clock expiry changed transaction terms or retained bytes", expired, err)
	}
	var reminders int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM album_retention_reminders WHERE owner_id=? AND state='pending' AND retention_deadline=?`, owner, expired.RetentionDeadline).Scan(&reminders); err != nil || reminders != 3 {
		t.Fatal("server refresh did not schedule reminders", reminders, err)
	}
	if _, err := r.ApplySubscription(ctx, owner, expired.Revision, staleActive, now); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("same signature reactivated expired capacity", err)
	}
}

func TestMySQLAlbumSubscriptionRefreshFreshnessFailureAndRecovery(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	owner := uuid.NewString()
	repository := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	if err := repository.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	active := fixture.payload(owner)
	client := &albumStatusStub{states: []appstore.SubscriptionStatus{{Status: 1, SignedTransaction: fixture.sign(t, active)}}}
	resolver, _ := NewAlbumSubscriptionResolver(fixture.verifier(), client, "Sandbox", func() time.Time { return now })
	service, _ := NewAlbumSubscriptionService(repository, resolver, func() time.Time { return now })
	a, err := service.Sync(ctx, owner, fixture.sign(t, active))
	if err != nil {
		t.Fatal(err)
	}
	_, _, objects, _, manifest := uploadFixture(t)
	uploads, _ := NewUploadService(repository, objects, UploadPolicy{1 << 30, 1 << 20, 4 * time.Hour, time.Minute}, func() time.Time { return now })
	u, err := uploads.Create(ctx, owner, uuid.NewString(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(albumSubscriptionFreshness)
	a, err = repository.CloudAccount(ctx, owner, now)
	if err != nil || !a.VerificationPending || a.QuotaBytes != 50_000_000_000 {
		t.Fatal("stale projection", a, err)
	}
	freshManifest := manifest
	freshManifest.SourceRevision = "stale-new-file"
	if _, err := uploads.Create(ctx, owner, uuid.NewString(), freshManifest); !errors.Is(err, ErrSubscriptionStale) {
		t.Fatal("stale capacity admitted bytes", err)
	}
	if _, err := uploads.Grants(ctx, owner, u.ID); !errors.Is(err, ErrSubscriptionStale) {
		t.Fatal("stale capacity issued PUT", err)
	}
	client.err = errors.New("provider offline")
	if found, err := service.refreshOne(ctx, repository); !found || !errors.Is(err, ErrSubscriptionUnavailable) {
		t.Fatal("failed refresh", found, err)
	}
	a, err = repository.CloudAccount(ctx, owner, now)
	if err != nil || a.Revision != 1 || !a.VerificationPending || a.ReservedBytes != u.OriginalBytes+u.DerivedBudget {
		t.Fatal("failure inferred deletion, expiry or freshness", a, err)
	}
	if found, err := service.refreshOne(ctx, repository); found || err != nil {
		t.Fatal("retry backoff ignored", found, err)
	}
	now = now.Add(time.Minute)
	client.err = nil
	if found, err := service.refreshOne(ctx, repository); !found || err != nil {
		t.Fatal("refresh recovery", found, err)
	}
	a, err = repository.CloudAccount(ctx, owner, now)
	if err != nil || a.VerificationPending || a.Revision != 2 {
		t.Fatal("verified refresh did not restore freshness", a, err)
	}
	if _, err := uploads.Create(ctx, owner, uuid.NewString(), freshManifest); err != nil {
		t.Fatal("recovered capacity did not resume", err)
	}
	// No notification or client login is required for periodic refund detection.
	refund := fixture.payload(owner)
	refund["revocationDate"] = fixture.now.Add(-time.Minute).UnixMilli()
	refund["signedDate"] = fixture.now.Add(time.Second).UnixMilli()
	client.states = []appstore.SubscriptionStatus{{Status: 5, SignedTransaction: fixture.sign(t, refund)}}
	now = now.Add(albumSubscriptionRefreshInterval)
	if found, err := service.refreshOne(ctx, repository); !found || err != nil {
		t.Fatal("periodic refund", found, err)
	}
	a, err = repository.CloudAccount(ctx, owner, now)
	if err != nil || a.QuotaBytes != 0 || a.State != "expired" || a.ReservedBytes == 0 {
		t.Fatal("periodic refund lost data", a, err)
	}
}

func TestMySQLAlbumSubscriptionRefreshLeaseFencesCrashAndClientChange(t *testing.T) {
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := uuid.NewString()
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return now })
	if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	v := VerifiedAlbumSubscription{"refresh-chain", "refresh-transaction", Cloud50GBAnnualProduct, "Sandbox", now.Add(time.Hour), now, true}
	if _, err := r.ApplySubscription(ctx, owner, 0, v, now); err != nil {
		t.Fatal(err)
	}
	forceDue := func() {
		if _, err := db.ExecContext(ctx, `UPDATE album_cloud_subscriptions SET next_check_at=? WHERE owner_id=?`, now, owner); err != nil {
			t.Fatal(err)
		}
	}
	forceDue()
	claims := make(chan albumSubscriptionRefreshClaim, 20)
	failures := make(chan error, 20)
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			c, found, err := r.claimSubscriptionRefresh(ctx, now)
			if err != nil {
				failures <- err
			}
			if found {
				claims <- c
			}
		}()
	}
	group.Wait()
	close(claims)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatal("one chain claimed concurrently", len(claims))
	}
	old := <-claims
	now = old.Until.Add(time.Microsecond)
	replacement, found, err := r.claimSubscriptionRefresh(ctx, now)
	if err != nil || !found || replacement.Token == old.Token {
		t.Fatal("abandoned lease not recoverable", found, err)
	}
	if err := r.finishSubscriptionRefresh(ctx, old, v, now); !errors.Is(err, ErrLease) {
		t.Fatal("old lease changed state", err)
	}
	if err := r.retrySubscriptionRefresh(ctx, old, now); !errors.Is(err, ErrLease) {
		t.Fatal("old lease changed retry schedule", err)
	}
	if err := r.finishSubscriptionRefresh(ctx, replacement, v, now); err != nil {
		t.Fatal(err)
	}
	forceDue()
	c, found, err := r.claimSubscriptionRefresh(ctx, now)
	if err != nil || !found {
		t.Fatal(err)
	}
	upgraded := v
	upgraded.ProductID = Cloud500GBAnnualProduct
	upgraded.TransactionID = "upgraded"
	upgraded.SignedAt = now
	a, err := r.CloudAccount(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	a, err = r.ApplySubscription(ctx, owner, a.Revision, upgraded, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.finishSubscriptionRefresh(ctx, c, v, now); !errors.Is(err, ErrLease) {
		t.Fatal("late worker replaced client upgrade", err)
	}
	if got, err := r.CloudAccount(ctx, owner, now); err != nil || got.QuotaBytes != 500_000_000_000 || got.Revision != a.Revision {
		t.Fatal("worker lost new client state", got, err)
	}
	production := NewMySQLUploads(db).WithSubscriptionEnvironment("Production", func() time.Time { return now })
	if _, found, err := production.claimSubscriptionRefresh(ctx, now); err != nil || found {
		t.Fatal("Sandbox anchors crossed into production", found, err)
	}
}
