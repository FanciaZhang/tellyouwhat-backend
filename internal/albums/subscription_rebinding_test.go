package albums

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type bindingProvider struct {
	t           *testing.T
	fixture     albumReceiptFixture
	payload     map[string]any
	target      string
	calls, puts int
	mode        string
	extra       []appstore.SubscriptionStatus
	beforePut   func()
	afterPut    func()
	afterRead   func(string)
}

func (p *bindingProvider) AllSubscriptionStatuses(_ context.Context, transaction string) ([]appstore.SubscriptionStatus, error) {
	p.calls++
	if p.mode == "read-error" {
		return nil, ErrSubscriptionUnavailable
	}
	signed := p.fixture.sign(p.t, p.payload)
	if p.mode == "forgery" {
		signed += "broken"
	}
	if p.beforePut != nil {
		f := p.beforePut
		p.beforePut = nil
		f()
	}
	if p.afterRead != nil {
		p.afterRead(transaction)
	}
	return append([]appstore.SubscriptionStatus{{Status: 1, SignedTransaction: signed}}, p.extra...), nil
}
func (p *bindingProvider) SetAppAccountToken(_ context.Context, original, token string) error {
	p.puts++
	if original != p.payload["originalTransactionId"] || token != p.target {
		p.t.Fatal("wrong binding destination")
	}
	if p.mode != "not-visible" {
		p.payload["appAccountToken"] = token
	}
	if p.afterPut != nil {
		p.afterPut()
	}
	if p.mode == "lost-reply" {
		return ErrSubscriptionUnavailable
	}
	return nil
}

func bindingFixture(t *testing.T, mode string) (*pushFixture, *MySQLUploads, *AlbumSubscriptionService, *bindingProvider, accountDeletionRequest) {
	t.Helper()
	f := newPushFixture(t)
	f.now = f.now.Truncate(time.Millisecond)
	ctx := context.Background()
	session, login := f.login(t, "binding-owner", f.now)
	repo := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return f.now })
	receipt := newAlbumReceiptFixture(t, f.now)
	payload := receipt.payload(login.AccountID)
	proof := VerifiedAlbumSubscription{"1000000000000001", "1000000000000002", Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(1, 0, 0), f.now, true}
	a, err := repo.ApplySubscription(ctx, login.AccountID, 0, proof, f.now)
	if err != nil {
		t.Fatal(err)
	}
	p := &bindingProvider{t: t, fixture: receipt, payload: payload, target: a.AppAccountToken, mode: mode}
	switch mode {
	case "stable":
		p.payload["appAccountToken"] = a.AppAccountToken
	case "foreign":
		p.payload["appAccountToken"] = uuid.NewString()
	case "missing":
		delete(p.payload, "appAccountToken")
	case "family":
		p.payload["inAppOwnershipType"] = "FAMILY_SHARED"
	case "stale":
		p.payload["signedDate"] = f.now.Add(-time.Minute).UnixMilli()
	}
	resolver, _ := NewAlbumSubscriptionResolver(receipt.verifier(), p, "Sandbox", func() time.Time { return f.now })
	service, _ := NewAlbumSubscriptionService(repo, resolver, func() time.Time { return f.now })
	d := acceptAppleDeletion(t, f, session.Token)
	return f, repo, service, p, d
}
func bindingAcknowledged(t *testing.T, db *sql.DB, owner string) bool {
	t.Helper()
	var at sql.NullTime
	if err := db.QueryRow(`SELECT acknowledged_at FROM album_deletion_subscription_jobs WHERE account_id=?`, owner).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.Valid
}

func TestMySQLSubscriptionBindingRequiresCurrentSignedReceipt(t *testing.T) {
	for _, mode := range []string{"legacy", "stable", "foreign", "missing", "family", "forgery", "read-error", "stale", "not-visible", "lost-reply"} {
		t.Run(mode, func(t *testing.T) {
			f, r, s, p, d := bindingFixture(t, mode)
			b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10)
			success := mode == "legacy" || mode == "stable"
			if err != nil || (b.Acknowledged == 1) != success || (b.Deferred == 1) == success || bindingAcknowledged(t, f.r.db, d.AccountID) != success {
				t.Fatal(b, err)
			}
			wantPuts := 0
			if mode == "legacy" || mode == "not-visible" || mode == "lost-reply" {
				wantPuts = 1
			}
			if p.puts != wantPuts {
				t.Fatal("unverified mutation", p.puts, wantPuts)
			}
			before := p.calls
			if b, err = s.runSubscriptionBindingBatch(context.Background(), r, 10); err != nil || b != (subscriptionBindingBatch{}) || p.calls != before {
				t.Fatal("completed/failed job immediately repeated", b, err)
			}
			if mode == "lost-reply" {
				f.now = f.now.Add(time.Minute + time.Second)
				b, err = s.runSubscriptionBindingBatch(context.Background(), r, 10)
				if err != nil || b.Acknowledged != 1 || p.puts != 1 {
					t.Fatal("lost reply was not reconciled read-only", b, err, p.puts)
				}
			}
			var state string
			if err = f.r.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, d.AccountID).Scan(&state); err != nil || state != "deleting" {
				t.Fatal("binding marked erasure complete", err)
			}
		})
	}
}

func TestMySQLSubscriptionBindingFencesLateWorkerAndRollsBackReceipt(t *testing.T) {
	for _, mode := range []string{"stale-lease", "cancel", "receipt-failure", "newer-proof"} {
		t.Run(mode, func(t *testing.T) {
			f, r, s, p, d := bindingFixture(t, "legacy")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "stale-lease":
				p.beforePut = func() {
					if _, e := f.r.db.Exec(`UPDATE album_deletion_subscription_jobs SET lease_token=? WHERE account_id=?`, uuid.NewString(), d.AccountID); e != nil {
						t.Fatal(e)
					}
				}
			case "cancel":
				p.afterPut = cancel
			case "receipt-failure":
				if _, e := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs ADD CONSTRAINT reject_test_binding_receipt CHECK(acknowledged_at IS NULL)`); e != nil {
					t.Fatal(e)
				}
			case "newer-proof":
				p.afterPut = func() {
					if _, e := f.r.db.Exec(`UPDATE album_cloud_subscriptions SET signed_at=? WHERE owner_id=?`, f.now.Add(time.Second), d.AccountID); e != nil {
						t.Fatal(e)
					}
				}
			}
			b, err := s.runSubscriptionBindingBatch(ctx, r, 10)
			if mode == "stale-lease" {
				if err != nil || b.Contended != 1 || p.puts != 0 {
					t.Fatal(b, err, p.puts)
				}
			} else if err == nil && !(mode == "newer-proof" && b.Deferred == 1) {
				t.Fatal("interrupted/failed acknowledgement succeeded")
			}
			if bindingAcknowledged(t, f.r.db, d.AccountID) {
				t.Fatal("incomplete receipt persisted")
			}
			if mode == "receipt-failure" {
				if _, e := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs DROP CHECK reject_test_binding_receipt`); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "newer-proof" {
				p.payload["signedDate"] = f.now.Add(time.Second).UnixMilli()
			}
			f.now = f.now.Add(3 * time.Minute)
			p.afterPut = nil
			p.beforePut = nil
			b, err = s.runSubscriptionBindingBatch(context.Background(), r, 10)
			if err != nil || b.Acknowledged != 1 {
				t.Fatal("restart failed to reconcile", b, err)
			}
			if p.puts != 1 {
				t.Fatal("duplicated successful mutation", p.puts)
			}
		})
	}
}

func TestMySQLSubscriptionBindingConcurrentClaimsAndWrongIntent(t *testing.T) {
	f, r, _, p, d := bindingFixture(t, "legacy")
	base := subscriptionBindingJob{Account: d.AccountID, Request: d.RequestID, Environment: "Sandbox", Original: "1000000000000001", Target: p.target}
	for _, mode := range []string{"wrong-request", "wrong-target", "wrong-account", "wrong-environment"} {
		j := base
		switch mode {
		case "wrong-request":
			j.Request = uuid.NewString()
		case "wrong-target":
			j.Target = uuid.NewString()
		case "wrong-account":
			j.Account = uuid.NewString()
		case "wrong-environment":
			j.Environment = "Production"
		}
		if _, err := r.claimSubscriptionBinding(context.Background(), j, f.now); err == nil {
			t.Fatal("invalid binding claim", mode)
		}
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.claimSubscriptionBinding(context.Background(), base, f.now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	won, contended := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, ErrLease) {
			contended++
		} else {
			t.Fatal(err)
		}
	}
	if won != 1 || contended != 1 {
		t.Fatal(won, contended)
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs RENAME COLUMN target_token TO hidden_target_token`); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckSubscriptionSchema(context.Background()); err == nil {
		t.Fatal("missing binding migration accepted")
	}
}

func TestMySQLSubscriptionBindingEnqueuesLaterVerifiedChain(t *testing.T) {
	f, r, s, p, d := bindingFixture(t, "stable")
	if b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	p.extra = []appstore.SubscriptionStatus{extraBindingStatus(t, p, "1000000000000001", p.target)}
	p.payload["originalTransactionId"] = "new-original"
	p.payload["transactionId"] = "new-transaction"
	if err := s.receiveNotification(context.Background(), p.fixture.notification(t, uuid.NewString(), "DID_RENEW", p.payload)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND request_id=? AND target_token=? AND acknowledged_at IS NULL`, d.AccountID, d.RequestID, p.target).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if b, err := s.runSubscriptionBindingBatch(context.Background(), r, 10); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	if err := s.receiveNotification(context.Background(), p.fixture.notification(t, uuid.NewString(), "DID_RENEW", p.payload)); err != nil {
		t.Fatal(err)
	}
	if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NULL`, d.AccountID).Scan(&count); err != nil || count != 1 {
		t.Fatal("later verified update did not invalidate only its own binding receipt", count, err)
	}
	if _, err := r.ApplySubscription(context.Background(), d.AccountID, 2, VerifiedAlbumSubscription{}, f.now); !errors.Is(err, ErrOwner) {
		t.Fatal("client sync reopened deleting account", err)
	}
}

func TestGatewaySubscriptionRebindingRunsWithExplicitEnable(t *testing.T) {
	f := newAppleGatewayFixture(t)
	ctx := context.Background()
	session, login, _ := f.login(t, "gateway-binding")
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := newAlbumReceiptFixture(t, now)
	repo := NewMySQLUploads(f.db).WithSubscriptionEnvironment("Sandbox")
	a, err := repo.ApplySubscription(ctx, login.AccountID, 0, VerifiedAlbumSubscription{"1000000000000001", "1000000000000002", Cloud50GBAnnualProduct, "Sandbox", now.AddDate(1, 0, 0), now, true}, now)
	if err != nil {
		t.Fatal(err)
	}
	f.delete(t, session)
	f.config.Subscriptions = &AlbumSubscriptionConfig{BundleID: albumDevelopmentAudience, RebindingEnabled: true, Environment: "Sandbox", IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: fixture.roots}
	var bound atomic.Bool
	var puts atomic.Int32
	base := f.transport(t, func(*http.Request) (*http.Response, error) { return appleResponse(`{}`), nil })
	transport := appleTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.storekit-sandbox.apple.com" {
			return base.RoundTrip(req)
		}
		if req.Method == "PUT" && req.URL.Path == "/inApps/v1/transactions/1000000000000001/appAccountToken" {
			var body map[string]string
			data, _ := io.ReadAll(req.Body)
			if json.Unmarshal(data, &body) != nil || len(body) != 1 || body["appAccountToken"] != a.AppAccountToken {
				t.Error("wrong configured binding")
			}
			puts.Add(1)
			bound.Store(true)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
		}
		if req.Method != "GET" || req.URL.Path != "/inApps/v1/subscriptions/1000000000000002" {
			t.Error("unexpected provider operation")
			return nil, ErrSubscriptionUnavailable
		}
		token := login.AccountID
		if bound.Load() {
			token = a.AppAccountToken
		}
		data, _ := json.Marshal(map[string]any{"bundleId": albumDevelopmentAudience, "environment": "Sandbox", "data": []any{map[string]any{"lastTransactions": []any{map[string]any{"status": 1, "signedTransactionInfo": fixture.sign(t, fixture.payload(token))}}}}})
		return appleResponse(string(data)), nil
	})
	g, err := NewGateway(ctx, f.config, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	deadline := time.Now().Add(8 * time.Second)
	for !bindingAcknowledged(t, f.db, login.AccountID) {
		if time.Now().After(deadline) {
			t.Fatal("configured worker did not acknowledge")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if puts.Load() != 1 {
		t.Fatal("unexpected configured PUT count", puts.Load())
	}
	g.Close()
}
