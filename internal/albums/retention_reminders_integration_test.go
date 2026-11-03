package albums

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func reminderRepository(t *testing.T) (*MySQLUploads, string, time.Time, VerifiedAlbumSubscription) {
	t.Helper()
	db, _ := gatewayTestDatabase(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal("migration replay", err)
	}
	r := NewMySQLUploads(db).WithSubscriptionEnvironment("Sandbox")
	if err := r.CheckSubscriptionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	// Account usage stands for existing verified media; reminder tests never create or delete objects.
	if _, err := db.ExecContext(ctx, `UPDATE album_storage_accounts SET used_bytes=100 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 123456000, time.UTC)
	v := VerifiedAlbumSubscription{"chain-" + uuid.NewString(), "transaction-1", Cloud50GBAnnualProduct, "Sandbox", time.Date(2026, 8, 31, 12, 0, 0, 123456000, time.UTC), now.Add(-time.Minute), false}
	return r, owner, now, v
}
func applyReminderSubscription(t *testing.T, r *MySQLUploads, owner string, v VerifiedAlbumSubscription, now time.Time) AlbumCloudAccount {
	t.Helper()
	a, err := r.CloudAccount(context.Background(), owner, now)
	if err != nil {
		t.Fatal(err)
	}
	a, err = r.ApplySubscription(context.Background(), owner, a.Revision, v, now)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func takeReminder(t *testing.T, r *MySQLUploads, now time.Time, days int) albumRetentionReminder {
	t.Helper()
	c, found, err := r.claimRetentionReminder(context.Background(), now)
	if err != nil || !found || c.Days != days {
		t.Fatalf("claim %d: found=%v err=%v stage=%d", days, found, err, c.Days)
	}
	return c
}
func noReminder(t *testing.T, r *MySQLUploads, now time.Time) {
	t.Helper()
	_, found, err := r.claimRetentionReminder(context.Background(), now)
	if err != nil || found {
		t.Fatal("unexpected reminder", found, err)
	}
}
func TestMySQLAlbumRetentionReminderWindowsRetryAndRestart(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	ctx := context.Background()
	a := applyReminderSubscription(t, r, owner, v, now)
	deadline := time.Date(2027, 2, 28, 12, 0, 0, 123456000, time.UTC)
	if a.RetentionDeadline == nil || !a.RetentionDeadline.Equal(deadline) {
		t.Fatal("calendar deadline", a.RetentionDeadline)
	}
	applyReminderSubscription(t, r, owner, v, now.Add(time.Minute))
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders`).Scan(&count); err != nil || count != 3 {
		t.Fatal("duplicate schedule", count, err)
	}
	due := deadline.AddDate(0, 0, -30)
	noReminder(t, r, due.Add(-time.Microsecond))
	first := takeReminder(t, r, due, 30)
	reopened := NewMySQLUploads(r.db).WithSubscriptionEnvironment("Sandbox")
	noReminder(t, reopened, due.Add(30*time.Second))
	second := takeReminder(t, reopened, due.Add(time.Minute), 30)
	if first.ID != second.ID || first.Token == second.Token || second.Attempts != 2 {
		t.Fatal("restart lease identity")
	}
	if err := r.finishRetentionReminder(ctx, first, true, due.Add(time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatal("stale claim finished", err)
	}
	if err := r.finishRetentionReminder(ctx, second, false, due.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	noReminder(t, r, due.Add(3*time.Minute-time.Microsecond))
	third := takeReminder(t, r, due.Add(3*time.Minute), 30)
	if err := r.finishRetentionReminder(ctx, third, true, due.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	applyReminderSubscription(t, r, owner, v, due.Add(4*time.Minute))
	noReminder(t, r, due.Add(5*time.Minute))
	week := takeReminder(t, r, deadline.AddDate(0, 0, -7), 7)
	if err := r.finishRetentionReminder(ctx, week, true, deadline.AddDate(0, 0, -7)); err != nil {
		t.Fatal(err)
	}
	day := takeReminder(t, r, deadline.AddDate(0, 0, -1), 1)
	if err := r.finishRetentionReminder(ctx, day, true, deadline.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	noReminder(t, r, deadline)
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders WHERE state='delivered' AND delivered_at IS NOT NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatal("receipts", count, err)
	}
	account, err := r.CloudAccount(ctx, owner, deadline)
	if err != nil || account.UsedBytes != 100 || account.State != "retention_due" {
		t.Fatal("reminders changed storage", err)
	}
}
func TestMySQLAlbumRetentionReminderRenewalInvalidatesClaim(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	ctx := context.Background()
	a := applyReminderSubscription(t, r, owner, v, now)
	due := a.RetentionDeadline.AddDate(0, 0, -30)
	claim := takeReminder(t, r, due, 30)
	renewed := v
	renewed.Active = true
	renewed.TransactionID = "renewed"
	renewed.ExpiresAt = due.AddDate(1, 0, 0)
	renewed.SignedAt = due
	applyReminderSubscription(t, r, owner, renewed, due)
	if err := r.finishRetentionReminder(ctx, claim, true, due.Add(time.Second)); !errors.Is(err, ErrLease) {
		t.Fatal("renewed account accepted old receipt", err)
	}
	noReminder(t, r, due.Add(2*time.Minute))
	var cancelled int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders WHERE state='cancelled' AND lease_token IS NULL`).Scan(&cancelled); err != nil || cancelled != 3 {
		t.Fatal("renewal cancellation", cancelled, err)
	}
	renewed.Active = false
	a = applyReminderSubscription(t, r, owner, renewed, renewed.ExpiresAt)
	next := takeReminder(t, r, a.RetentionDeadline.AddDate(0, 0, -30), 30)
	if next.ID == claim.ID || next.Deadline.Equal(claim.Deadline) {
		t.Fatal("renewed deadline reused old event")
	}
	noReminder(t, NewMySQLUploads(r.db).WithSubscriptionEnvironment("Production"), a.RetentionDeadline.AddDate(0, 0, -30))
	forged := next
	forged.Owner = uuid.NewString()
	if err := r.finishRetentionReminder(ctx, forged, true, a.RetentionDeadline.AddDate(0, 0, -30)); err == nil {
		t.Fatal("foreign owner acknowledged event")
	}
}
func TestMySQLAlbumRetentionReminderDowntimeAndConcurrentClaim(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	ctx := context.Background()
	a := applyReminderSubscription(t, r, owner, v, now)
	now = a.RetentionDeadline.AddDate(0, 0, -2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claims []albumRetentionReminder
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, found, err := r.claimRetentionReminder(ctx, now)
			if err != nil {
				t.Error(err)
			}
			if found {
				mu.Lock()
				claims = append(claims, c)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claims) != 1 || claims[0].Days != 7 {
		t.Fatal("concurrent or obsolete reminder", len(claims))
	}
	// A delayed acknowledgement cannot deliver the previous stage after the next window begins.
	if err := r.finishRetentionReminder(ctx, claims[0], true, a.RetentionDeadline.AddDate(0, 0, -1)); !errors.Is(err, ErrLease) {
		t.Fatal("late stage acknowledged", err)
	}
	day := takeReminder(t, r, a.RetentionDeadline.AddDate(0, 0, -1), 1)
	if err := r.finishRetentionReminder(ctx, day, true, *a.RetentionDeadline); !errors.Is(err, ErrLease) {
		t.Fatal("deadline acknowledged", err)
	}
	noReminder(t, r, *a.RetentionDeadline)
	var pending int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders WHERE state='pending'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("expired queue remains pending", pending, err)
	}
}
func TestMySQLAlbumRetentionReminderSchedulingRollsBackSubscription(t *testing.T) {
	r, owner, now, v := reminderRepository(t)
	ctx := context.Background()
	_, err := r.db.Exec(`ALTER TABLE album_retention_reminders ADD CONSTRAINT reject_reminder CHECK(days_before<>7)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplySubscription(ctx, owner, 0, v, now); err == nil {
		t.Fatal("partial subscription committed")
	}
	for _, table := range []string{"album_retention_reminders", "album_cloud_subscriptions"} {
		var count int
		if err := r.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial rows", table, count, err)
		}
	}
	a, err := r.CloudAccount(ctx, owner, now)
	if err != nil || a.Revision != 0 || a.State != "none" {
		t.Fatal("revision committed", a.Revision, err)
	}
	if _, err := r.db.Exec(`ALTER TABLE album_retention_reminders DROP CHECK reject_reminder`); err != nil {
		t.Fatal(err)
	}
	// Empty accounts need no deletion warning; repeated reconciliation can schedule if media remains later.
	if _, err := r.db.Exec(`UPDATE album_storage_accounts SET used_bytes=0 WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	applyReminderSubscription(t, r, owner, v, now)
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM album_retention_reminders`).Scan(&count); err != nil || count != 0 {
		t.Fatal("empty account reminder", count, err)
	}
}
