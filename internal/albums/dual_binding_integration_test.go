package albums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type dualBindingFixture struct {
	f         *pushFixture
	r         *MySQLUploads
	s         *AlbumSubscriptionService
	providers map[string]*bindingProvider
	d         accountDeletionRequest
}

func newDualBindingFixture(t *testing.T) dualBindingFixture {
	t.Helper()
	f := newPushFixture(t)
	f.now = f.now.Truncate(time.Millisecond)
	f.accounts.bundleID = albumAppleAudience
	ctx := context.Background()
	login, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Audience: albumAppleAudience, Subject: "dual-binding-owner", IssuedAt: f.now}, RefreshToken: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.sessions.Issue(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	receipt := newAlbumReceiptFixture(t, f.now)
	providers := map[string]*bindingProvider{}
	providerLocks := map[string]*sync.Mutex{"Production": {}, "Sandbox": {}}
	transport := appleTestTransport(func(req *http.Request) (*http.Response, error) {
		environment := "Production"
		if req.URL.Host == "api.storekit-sandbox.apple.com" {
			environment = "Sandbox"
		} else if req.URL.Host != "api.storekit.apple.com" {
			return nil, ErrSubscriptionUnavailable
		}
		providerLocks[environment].Lock()
		defer providerLocks[environment].Unlock()
		p := providers[environment]
		if req.Method == http.MethodPut {
			var body map[string]string
			if !strings.HasSuffix(req.URL.Path, "/appAccountToken") || json.NewDecoder(req.Body).Decode(&body) != nil || len(body) != 1 {
				return nil, ErrSubscriptionBinding
			}
			if err := p.SetAppAccountToken(ctx, path.Base(path.Dir(req.URL.Path)), body["appAccountToken"]); err != nil {
				return nil, err
			}
			return appleResponse(""), nil
		}
		if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.Path, "/inApps/v1/subscriptions/") {
			return nil, ErrSubscriptionUnavailable
		}
		states, err := p.AllSubscriptionStatuses(ctx, path.Base(req.URL.Path))
		if err != nil {
			return nil, err
		}
		var transactions []any
		for _, state := range states {
			transactions = append(transactions, map[string]any{"status": state.Status, "signedTransactionInfo": state.SignedTransaction})
		}
		data, _ := json.Marshal(map[string]any{"bundleId": albumAppleAudience, "appAppleId": 12345, "environment": environment, "data": []any{map[string]any{"lastTransactions": transactions}}})
		return appleResponse(string(data)), nil
	})
	r := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Production", func() time.Time { return f.now })
	s, err := newConfiguredAlbumSubscriptions(r, &AlbumSubscriptionConfig{BundleID: albumAppleAudience, Environment: "Both", AppAppleID: 12345, SandboxQuotaBytes: 1_000_000_000, IssuerID: uuid.NewString(), KeyID: "ALBUMSKEY1", SigningKey: albumTestECKey(t), Roots: receipt.roots, RebindingEnabled: true, RecordErasureEnabled: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []*AlbumSubscriptionService{s, s.sandbox} {
		service.now = func() time.Time { return f.now }
		service.resolver.now = service.now
		env := service.resolver.environment
		payload := receipt.payload(login.AccountID)
		payload["environment"], payload["bundleId"] = env, albumAppleAudience
		providers[env] = &bindingProvider{t: t, fixture: receipt, payload: payload}
		// Deliberately identical transaction IDs across environments.
		a, err := service.Sync(ctx, login.AccountID, receipt.sign(t, payload))
		if err != nil {
			t.Fatal(err)
		}
		providers[env].target = a.AppAccountToken
	}
	d := acceptAppleDeletion(t, f, session.Token)
	return dualBindingFixture{f: f, r: r, s: s, providers: providers, d: d}
}

func (x dualBindingFixture) readyForRecordErasure(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	media := sharingFixture{db: x.f.r.db, repo: x.r, login: AccountLogin{AccountID: x.d.AccountID}, now: &x.f.now}
	storage := readyAccountMediaSettlement(t, media, x.d.RequestID)
	if _, err := x.r.settleAccountMediaPurge(ctx, x.d.AccountID, x.d.RequestID, storage, func() time.Time { return x.f.now }); err != nil {
		t.Fatal(err)
	}
	if b, err := x.f.accounts.runAppleDeletionBatch(ctx, func(context.Context, string) error { return nil }, 10, func() time.Time { return x.f.now }); err != nil || b.Acknowledged != 1 {
		t.Fatal(b, err)
	}
	if _, err := x.f.accounts.eraseAccountAccess(ctx, x.d.AccountID, x.d.RequestID, x.f.now); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLDualBindingCompletionAndRecordErasure(t *testing.T) {
	for _, last := range []string{"Production", "Sandbox"} {
		t.Run(last, func(t *testing.T) {
			x := newDualBindingFixture(t)
			first, final := x.s, x.s.sandbox
			if last == "Production" {
				first, final = final, first
			}
			ctx := context.Background()
			if b, err := first.runSubscriptionBindingBatch(ctx, first.repository.(*MySQLUploads), 10); err != nil || b.Acknowledged != 1 {
				t.Fatal(b, err)
			}
			if revision, checked, _ := readCompletionReceipt(t, x.f, x.d.AccountID); revision.Valid || checked.Valid {
				t.Fatal("one environment authorized full erasure")
			}
			if b, err := final.runSubscriptionBindingBatch(ctx, final.repository.(*MySQLUploads), 10); err != nil || b.Acknowledged != 1 {
				t.Fatal(b, err)
			}
			for env, p := range x.providers {
				if p.puts != 1 || p.calls < 4 {
					t.Fatal("chain not rebound and rechecked", env, p.puts, p.calls)
				}
			}
			x.readyForRecordErasure(t)
			// A single-environment repository must not consume a dual receipt.
			single := *x.r
			single.productionSandboxQuota = 0
			if _, err := single.eraseAccountRecords(ctx, x.d.AccountID, x.d.RequestID, x.s.now); !errors.Is(err, ErrDerivedPending) {
				t.Fatal("single environment erased peer chains", err)
			}
			if count, err := x.r.runAccountRecordErasureBatch(ctx, 10, x.s.now); err != nil || count != 1 {
				t.Fatal("dual erasure", count, err)
			}
			for _, query := range []string{`SELECT COUNT(*) FROM album_accounts WHERE account_id=?`, `SELECT COUNT(*) FROM album_cloud_subscriptions WHERE owner_id=?`, `SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=?`} {
				var count int
				if err := x.f.r.db.QueryRow(query, x.d.AccountID).Scan(&count); err != nil || count != 0 {
					t.Fatal("partial account erasure", count, err)
				}
			}
			if _, err := x.r.erasedAccountRecords(ctx, x.d.AccountID, x.d.RequestID); err != nil {
				t.Fatal("missing durable erasure receipt", err)
			}
		})
	}
}

func TestMySQLDualBindingCompletionRejectsIncompletePeer(t *testing.T) {
	for _, mode := range []string{"provider-failure", "wrong-environment", "legacy-token", "missing-resolver", "new-chain", "revision-race"} {
		t.Run(mode, func(t *testing.T) {
			x := newDualBindingFixture(t)
			ctx := context.Background()
			if b, err := x.s.runSubscriptionBindingBatch(ctx, x.r, 10); err != nil || b.Acknowledged != 1 {
				t.Fatal(b, err)
			}
			peer := x.providers["Production"]
			switch mode {
			case "provider-failure":
				peer.mode = "read-error"
			case "wrong-environment":
				peer.payload["environment"] = "Sandbox"
			case "legacy-token":
				peer.payload["appAccountToken"] = x.d.AccountID
			case "missing-resolver":
				x.s.sandbox.bindingPeer = nil
			case "new-chain":
				payload := peer.fixture.payload(peer.target)
				payload["environment"], payload["bundleId"] = "Production", albumAppleAudience
				payload["originalTransactionId"], payload["transactionId"] = "new-chain", "new-transaction"
				state := extraBindingStatus(t, peer, "new-chain", peer.target)
				state.SignedTransaction = peer.fixture.sign(t, payload)
				peer.extra = append(peer.extra, state)
			case "revision-race":
				peer.afterRead = func(string) {
					peer.afterRead = nil
					if _, err := x.f.r.db.Exec(`UPDATE album_storage_accounts SET subscription_revision=subscription_revision+1 WHERE owner_id=?`, x.d.AccountID); err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := x.s.sandbox.runSubscriptionBindingBatch(ctx, x.s.sandbox.repository.(*MySQLUploads), 10)
			if b.Acknowledged != 0 || (err != nil && !errors.Is(err, ErrSubscriptionConflict)) {
				t.Fatal("incomplete peer accepted", b, err)
			}
			if revision, checked, _ := readCompletionReceipt(t, x.f, x.d.AccountID); revision.Valid || checked.Valid {
				t.Fatal("incomplete peer left full completion receipt")
			}
			if mode == "new-chain" {
				var count int
				if err := x.f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND environment='Production' AND original_transaction_id='new-chain' AND acknowledged_at IS NULL`, x.d.AccountID).Scan(&count); err != nil || count != 1 {
					t.Fatal("peer discovery not queued in correct environment", count, err)
				}
			}
		})
	}
}

func TestMySQLDualBindingWorkersCompleteBothQueues(t *testing.T) {
	x := newDualBindingFixture(t)
	// Media settlement and access erasure are performed by their own workers.
	x.readyForRecordErasure(t)
	// Let the Production worker acknowledge first. The separate concurrent-pass
	// test covers the conflict that requires another leased verification pass.
	x.providers["Sandbox"].beforePut = func() {
		deadline := time.Now().Add(15 * time.Second)
		for {
			var count int
			if err := x.f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND environment='Production' AND acknowledged_at IS NOT NULL`, x.d.AccountID).Scan(&count); err != nil {
				t.Error(err)
				return
			}
			if count == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("Production worker did not acknowledge")
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	g := &Gateway{}
	g.startSubscriptionRefresh(context.Background(), x.s, x.r)
	stop := func() { g.BeginDrain(); g.subscriptionWorkers.Wait() }
	defer stop()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := x.r.erasedAccountRecords(context.Background(), x.d.AccountID, x.d.RequestID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dual workers did not complete account erasure")
		}
		time.Sleep(25 * time.Millisecond)
	}
	stop()
	for env, p := range x.providers {
		if p.puts != 1 {
			t.Fatal("wrong background rebinding count", env, p.puts)
		}
	}
}

func TestMySQLDualBindingConcurrentPartialPassesNeedFreshCompletion(t *testing.T) {
	x := newDualBindingFixture(t)
	ctx := context.Background()
	services := []*AlbumSubscriptionService{x.s, x.s.sandbox}
	jobs := make([]subscriptionBindingJob, 2)
	for i, service := range services {
		env := service.resolver.environment
		x.providers[env].payload["appAccountToken"] = x.providers[env].target
		r := service.repository.(*MySQLUploads)
		j, err := r.claimSubscriptionBinding(ctx, subscriptionBindingJob{Account: x.d.AccountID, Request: x.d.RequestID, Environment: env, Original: "1000000000000001", Target: x.providers[env].target}, x.f.now)
		if err != nil {
			t.Fatal(err)
		}
		jobs[i] = j
		if proof, err := service.verifyBindingCompletion(ctx, r, j); err != nil || proof != nil {
			t.Fatal("concurrent partial verification marked complete", err)
		}
	}
	for i, service := range services {
		r, j := service.repository.(*MySQLUploads), jobs[i]
		proof, err := service.resolver.resolveCurrent(ctx, j.Account, j.Original, j.Transaction, j.Target)
		if err != nil {
			t.Fatal(err)
		}
		err = r.finishSubscriptionBinding(ctx, j, &proof, nil, x.f.now)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, ErrSubscriptionConflict) {
			t.Fatal("concurrent last acknowledgement bypassed complete verification", err)
		}
	}
	if revision, checked, _ := readCompletionReceipt(t, x.f, x.d.AccountID); revision.Valid || checked.Valid {
		t.Fatal("partial verification persisted full receipt")
	}
	r, j := x.s.sandbox.repository.(*MySQLUploads), jobs[1]
	complete, err := x.s.sandbox.verifyBindingCompletion(ctx, r, j)
	if err != nil || complete == nil {
		t.Fatal("fresh dual completion failed", err)
	}
	proof, err := x.s.sandbox.resolver.resolveCurrent(ctx, j.Account, j.Original, j.Transaction, j.Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.finishSubscriptionBinding(ctx, j, &proof, complete, x.f.now); err != nil {
		t.Fatal(err)
	}
}
