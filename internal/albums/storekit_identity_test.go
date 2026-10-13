package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLStoreKitIdentitySurvivesAccountRecreationWithoutKeepingOldAccount(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	repo := NewMySQLUploads(f.r.db).WithSubscriptionEnvironment("Sandbox", func() time.Time { return f.now })
	login := func(subject string) AccountLogin {
		a, err := f.accounts.RegisterAppleLogin(ctx, AppleGrant{Identity: AppleIdentity{Subject: subject, IssuedAt: f.now}, RefreshToken: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	old := login("same-verified-apple-user")
	other := login("different-verified-apple-user")
	initial, err := repo.CloudAccount(ctx, old.AccountID, f.now)
	if err != nil || !validOwner(initial.AppAccountToken) || initial.AppAccountToken == old.AccountID {
		t.Fatal(initial, err)
	}
	// Independent Go computation checks the MySQL-generated UUID bits and namespace.
	expected := uuid.NewSHA1(uuid.NameSpaceURL, []byte("cn.tellyouwhat.albums/storekit/v1:"+appleSubjectHash("same-verified-apple-user"))).String()
	if initial.AppAccountToken != expected {
		t.Fatal("SQL/UUID derivation mismatch")
	}
	fixture := newAlbumReceiptFixture(t, f.now)
	payload := fixture.payload(initial.AppAccountToken)
	signed := fixture.sign(t, payload)
	client := &albumStatusStub{states: []appstore.SubscriptionStatus{{Status: 1, SignedTransaction: signed}}}
	resolver, _ := NewAlbumSubscriptionResolver(fixture.verifier(), client, "Sandbox", func() time.Time { return f.now })
	service, _ := NewAlbumSubscriptionService(repo, resolver, func() time.Time { return f.now })
	if _, err = service.Sync(ctx, other.AccountID, signed); !errors.Is(err, ErrSubscriptionBinding) {
		t.Fatal("foreign identity admitted", err)
	}
	if a, e := service.Sync(ctx, old.AccountID, signed); e != nil || a.State != "active" {
		t.Fatal(a, e)
	}
	// Exercise the boundary after records are gone, not the unfinished erasure workflow.
	for _, q := range []string{`DELETE FROM album_retention_reminders WHERE owner_id=?`, `DELETE FROM album_cloud_subscriptions WHERE owner_id=?`, `DELETE FROM album_storage_accounts WHERE owner_id=?`, `DELETE FROM album_apple_grants WHERE account_id=?`, `DELETE FROM album_accounts WHERE account_id=?`} {
		if _, err = f.r.db.Exec(q, old.AccountID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = repo.SubscriptionOwner(ctx, initial.AppAccountToken); !errors.Is(err, ErrSubscriptionBinding) {
		t.Fatal("deleted account was addressable", err)
	}
	rebuilt := login("same-verified-apple-user")
	if rebuilt.AccountID == old.AccountID {
		t.Fatal("reused media/cache namespace")
	}
	a, err := repo.CloudAccount(ctx, rebuilt.AccountID, f.now)
	if err != nil || a.AppAccountToken != initial.AppAccountToken || a.State != "none" {
		t.Fatal("recreated account incorrectly restored data or changed token", a, err)
	}
	if owner, e := repo.SubscriptionOwner(ctx, a.AppAccountToken); e != nil || owner != rebuilt.AccountID {
		t.Fatal(owner, e)
	}
	a, err = service.Sync(ctx, rebuilt.AccountID, signed)
	if err != nil || a.State != "active" || a.UsedBytes != 0 || a.ReservedBytes != 0 {
		t.Fatal("same identity could not restore purchase", a, err)
	}
	if _, err = repo.SubscriptionOwner(ctx, old.AccountID); !errors.Is(err, ErrSubscriptionBinding) {
		t.Fatal("old account alias revived", err)
	}
	if _, err = service.Sync(ctx, other.AccountID, signed); !errors.Is(err, ErrSubscriptionBinding) {
		t.Fatal("restoration changed ownership", err)
	}
	// Notifications and background refresh must resolve the stable token to the new media owner.
	if err = service.receiveNotification(ctx, fixture.notification(t, uuid.NewString(), "DID_RENEW", payload)); err != nil {
		t.Fatal("notification binding", err)
	}
	if _, err = f.r.db.Exec(`UPDATE album_cloud_subscriptions SET next_check_at=? WHERE owner_id=?`, f.now, rebuilt.AccountID); err != nil {
		t.Fatal(err)
	}
	if found, e := service.refreshOne(ctx, repo); !found || e != nil {
		t.Fatal("background binding", found, e)
	}
	anchors := []reminderSubscriptionAnchor{{"1000000000000001", "1000000000000002"}}
	if v, e := service.resolveKnownSubscriptions(ctx, rebuilt.AccountID, anchors); e != nil || len(v) != 1 {
		t.Fatal("retention binding", e)
	}
	unknown := fixture.payload(a.AppAccountToken)
	unknown["originalTransactionId"] = "new-chain"
	unknown["transactionId"] = "new-transaction"
	client.states = append(client.states, appstore.SubscriptionStatus{Status: 1, SignedTransaction: fixture.sign(t, unknown)})
	if _, err = service.resolveKnownSubscriptions(ctx, rebuilt.AccountID, anchors); !errors.Is(err, ErrSubscriptionConflict) {
		t.Fatal("undiscovered stable-token subscription did not block retention", err)
	}
}

func TestMySQLStoreKitIdentityKeepsLegacyBindingAndRejectsMissingMigration(t *testing.T) {
	f, s := newAccountPurgeFixture(t)
	// A deleting account remains fenced even when its token is computable.
	a, err := f.repo.CloudAccount(context.Background(), f.login.AccountID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newAlbumReceiptFixture(t, *f.now)
	payload := fixture.payload(f.login.AccountID)
	signed := fixture.sign(t, payload)
	client := &albumStatusStub{states: []appstore.SubscriptionStatus{{Status: 1, SignedTransaction: signed}}}
	resolver, _ := NewAlbumSubscriptionResolver(fixture.verifier(), client, "Sandbox", func() time.Time { return *f.now })
	if _, err = resolver.Resolve(context.Background(), f.login.AccountID, signed, a.AppAccountToken); err != nil {
		t.Fatal("legacy proof lost", err)
	}
	service, _ := NewAlbumSubscriptionService(f.repo, resolver, func() time.Time { return *f.now })
	if _, err = service.Sync(context.Background(), f.login.AccountID, signed); err == nil {
		t.Fatal("deleting account revived")
	}
	if owner, e := f.repo.SubscriptionOwner(context.Background(), f.login.AccountID); e != nil || owner != f.login.AccountID {
		t.Fatal("legacy alias", e)
	}
	_ = s
	if _, err = f.db.Exec(`ALTER TABLE album_accounts RENAME COLUMN storekit_account_token TO hidden_storekit_account_token`); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.CheckSubscriptionSchema(context.Background()); err == nil {
		t.Fatal("missing migration accepted")
	}
}

func TestMySQLStoreKitIdentityMigrationPreservesExistingAccounts(t *testing.T) {
	f := newPushFixture(t)
	_, login := f.login(t, "existing-before-token-migration", f.now)
	ctx := context.Background()
	expected := uuid.NewSHA1(uuid.NameSpaceURL, []byte("cn.tellyouwhat.albums/storekit/v1:"+appleSubjectHash("existing-before-token-migration"))).String()
	if _, err := f.r.db.Exec(`ALTER TABLE album_accounts DROP INDEX album_storekit_identity,DROP COLUMN storekit_account_token`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.db.Exec(`DELETE FROM schema_migrations WHERE name='0058_album_storekit_account_token.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, f.r.db); err != nil {
			t.Fatal(err)
		}
	}
	var actual, owner string
	if err := f.r.db.QueryRow(`SELECT account_id,storekit_account_token FROM album_accounts WHERE account_id=?`, login.AccountID).Scan(&owner, &actual); err != nil || owner != login.AccountID || actual != expected {
		t.Fatal("existing account migration", err)
	}
	if _, err := f.accounts.RefreshCredential(ctx, login.AccountID, login.GrantID); err != nil {
		t.Fatal("migration damaged login grant", err)
	}
}
