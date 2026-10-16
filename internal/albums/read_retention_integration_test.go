package albums

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func expiredReadFixture(t *testing.T) (sharingFixture, VerifiedAlbumSubscription, time.Time) {
	t.Helper()
	f := newSharingFixture(t)
	var proof VerifiedAlbumSubscription
	if err := f.db.QueryRow(`SELECT original_transaction_id,transaction_id,product_id,environment FROM album_cloud_subscriptions WHERE owner_id=?`, f.login.AccountID).Scan(&proof.OriginalTransactionID, &proof.TransactionID, &proof.ProductID, &proof.Environment); err != nil {
		t.Fatal(err)
	}
	proof.ExpiresAt = f.now.AddDate(0, -6, 0).Add(time.Minute)
	proof.SignedAt = f.now.Add(time.Millisecond)
	account := applyReminderSubscription(t, f.repo, f.login.AccountID, proof, *f.now)
	deadline := *account.RetentionDeadline
	*f.now = deadline.Add(-time.Second)
	return f, proof, deadline
}
func TestMySQLCloudReadsStopAtRetentionWithoutBlockingExpiredExports(t *testing.T) {
	f, proof, deadline := expiredReadFixture(t)
	signer := &retainedReadSigner{AlbumObjects: f.uploads.objects}
	f.uploads.objects = signer
	before, _ := f.repo.CloudAccount(context.Background(), f.login.AccountID, *f.now)
	if before.State != "expired" || before.QuotaBytes != 0 || before.UsedBytes == 0 {
		t.Fatal("fixture did not lose paid upload rights")
	}
	if _, err := f.repo.CloudMediaGrantDeadline(context.Background(), f.login.AccountID, *f.now); !errors.Is(err, ErrSubscriptionRequired) {
		t.Fatal("expired upload rights remained", err)
	}
	f.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	grant, err := f.uploads.OriginalReadGrant(ctx, f.login.AccountID, f.version.VersionID, "photo")
	if err != nil || !grant.ExpiresAt.Equal(deadline) || len(signer.expiries) != 1 || !signer.expiries[0].Equal(deadline) {
		t.Fatal("expired export blocked or unbounded", err)
	}
	*f.now = deadline
	if _, err := f.uploads.OriginalReadGrant(ctx, f.login.AccountID, f.version.VersionID, "photo"); !errors.Is(err, ErrNotFound) || len(signer.expiries) != 1 {
		t.Fatal("read after final cutoff", err)
	}
	after, _ := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
	if after.UsedBytes != before.UsedBytes || after.Revision != before.Revision {
		t.Fatal("read mutated media/entitlement")
	}
	proof.Active = true
	proof.ExpiresAt = deadline.AddDate(1, 0, 0)
	proof.SignedAt = deadline.Add(time.Millisecond)
	applyReminderSubscription(t, f.repo, f.login.AccountID, proof, *f.now)
	if _, err := f.uploads.OriginalReadGrant(ctx, f.login.AccountID, f.version.VersionID, "photo"); err != nil || len(signer.expiries) != 2 {
		t.Fatal("renewed export did not recover", err)
	}
}
func TestMySQLCloudReadGrantSerializesEntitlementChanges(t *testing.T) {
	f, proof, deadline := expiredReadFixture(t)
	began, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		done <- f.repo.WithCloudReadGrant(ctx, f.login.AccountID, f.version, *f.now, deadline, func(time.Time) error {
			close(began)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-began:
	case <-ctx.Done():
		t.Fatal("authorization never acquired account")
	}
	account, err := f.repo.CloudAccount(ctx, f.login.AccountID, *f.now)
	if err != nil {
		t.Fatal(err)
	}
	updateCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = f.repo.ApplySubscription(updateCtx, f.login.AccountID, account.Revision, proof, *f.now)
	stop()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("entitlement mutation bypassed read lock", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.ApplySubscription(ctx, f.login.AccountID, account.Revision, proof, *f.now); err != nil {
		t.Fatal("read lock leaked", err)
	}
}
func TestMySQLCloudReadRejectsScopeStateAndSigningFailure(t *testing.T) {
	for _, name := range []string{"foreign-owner", "foreign-version", "changed-manifest", "disabled-account", "signing-error", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			f, _, deadline := expiredReadFixture(t)
			owner, v := f.login.AccountID, f.version
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "foreign-owner":
				owner = uuid.NewString()
			case "foreign-version":
				v.VersionID = uuid.NewString()
			case "changed-manifest":
				v.ManifestDigest = "changed"
			case "disabled-account":
				if _, err := f.db.Exec(`UPDATE album_accounts SET state='disabled' WHERE account_id=?`, owner); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			calls := 0
			err := f.repo.WithCloudReadGrant(ctx, owner, v, *f.now, deadline, func(time.Time) error { calls++; return errors.New("signing failed") })
			want := 0
			if name == "signing-error" {
				want = 1
			}
			if err == nil || calls != want {
				t.Fatal("unsafe read reached signing", calls, err)
			}
		})
	}
}
