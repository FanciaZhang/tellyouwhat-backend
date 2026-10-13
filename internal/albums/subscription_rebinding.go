package albums

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type subscriptionBindingJob struct {
	Account, Request, Environment, Original, Transaction, Target, Lease string
	Until, SignedAt                                                     time.Time
	Attempts                                                            int
}
type subscriptionBindingBatch struct{ Acknowledged, Deferred, Contended int }
type subscriptionTokenSetter interface {
	SetAppAccountToken(context.Context, string, string) error
}

// Called with the storage account locked. This also captures chains first
// discovered by a verified Apple notification after deletion was accepted.
func enqueueSubscriptionBindings(ctx context.Context, tx *sql.Tx, owner, environment, original string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO album_deletion_subscription_jobs(environment,original_transaction_id,account_id,request_id,target_token,available_at)
 SELECT c.environment,c.original_transaction_id,c.owner_id,d.request_id,a.storekit_account_token,?
 FROM album_cloud_subscriptions c JOIN album_account_deletions d ON d.account_id=c.owner_id
 JOIN album_accounts a ON a.account_id=c.owner_id JOIN album_storage_accounts s ON s.owner_id=c.owner_id
 WHERE c.owner_id=? AND (?='' OR c.environment=?) AND (?='' OR c.original_transaction_id=?) AND a.state='deleting' AND s.deletion_requested_at=d.requested_at
 ON DUPLICATE KEY UPDATE
 available_at=IF(acknowledged_at IS NOT NULL,VALUES(available_at),available_at),
 verified_transaction_id=NULL,verified_signed_at=NULL,acknowledged_at=NULL`, at, owner, environment, environment, original, original)
	return err
}

func (r *MySQLUploads) lockSubscriptionBinding(ctx context.Context, tx *sql.Tx, j subscriptionBindingJob) error {
	if !validOwner(j.Account) || !validOwner(j.Request) || j.Environment != r.billingEnvironment || j.Original == "" {
		return ErrSubscriptionBinding
	}
	if err := lockDeletingAccount(ctx, tx, j.Account, j.Request); err != nil {
		return err
	}
	if _, err := lockAccount(ctx, tx, j.Account); err != nil {
		return err
	}
	var expected string
	var requested time.Time
	var fenced sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT a.storekit_account_token,d.requested_at,s.deletion_requested_at FROM album_accounts a JOIN album_account_deletions d ON d.account_id=a.account_id JOIN album_storage_accounts s ON s.owner_id=a.account_id WHERE a.account_id=? AND d.request_id=?`, j.Account, j.Request).Scan(&expected, &requested, &fenced); err != nil {
		return err
	}
	if expected != j.Target || !validOwner(expected) || expected == j.Account || !fenced.Valid || !requested.Equal(fenced.Time) {
		return ErrSubscriptionBinding
	}
	return nil
}

func (r *MySQLUploads) claimSubscriptionBinding(ctx context.Context, j subscriptionBindingJob, at time.Time) (subscriptionBindingJob, error) {
	at = at.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return j, ErrInvalidManifest
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	if err = r.lockSubscriptionBinding(ctx, tx, j); err != nil {
		return j, err
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT owner_id,transaction_id,signed_at FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? FOR UPDATE`, j.Environment, j.Original).Scan(&owner, &j.Transaction, &j.SignedAt); err != nil {
		return j, err
	}
	if owner != j.Account {
		return j, ErrSubscriptionBinding
	}
	j.Lease, j.Until = uuid.NewString(), at.Add(2*time.Minute)
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_subscription_jobs SET lease_token=?,lease_until=?,attempts=LEAST(attempts+1,1000000)
 WHERE environment=? AND original_transaction_id=? AND account_id=? AND request_id=? AND target_token=? AND acknowledged_at IS NULL AND available_at<=? AND (lease_until IS NULL OR lease_until<=?)`, j.Lease, j.Until, j.Environment, j.Original, j.Account, j.Request, j.Target, at, at)
	if err != nil {
		return j, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return j, err
	}
	if n != 1 {
		return j, ErrLease
	}
	if err = tx.QueryRowContext(ctx, `SELECT attempts FROM album_deletion_subscription_jobs WHERE environment=? AND original_transaction_id=?`, j.Environment, j.Original).Scan(&j.Attempts); err != nil {
		return j, err
	}
	return j, tx.Commit()
}

func (r *MySQLUploads) finishSubscriptionBinding(ctx context.Context, j subscriptionBindingJob, proof *VerifiedAlbumSubscription, completion *bindingCompletionProof, at time.Time) error {
	at = at.UTC().Truncate(time.Microsecond)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.lockSubscriptionBinding(ctx, tx, j); err != nil {
		return err
	}
	var receipt, transaction, signed any
	if proof != nil {
		snapshot, last, err := r.bindingCompletionSnapshot(ctx, tx, j)
		if err != nil {
			return err
		}
		if last && !matchingBindingCompletion(snapshot, completion, at) {
			return ErrSubscriptionConflict
		}
		var currentOwner string
		var currentSigned time.Time
		if err = tx.QueryRowContext(ctx, `SELECT owner_id,signed_at FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? FOR UPDATE`, j.Environment, j.Original).Scan(&currentOwner, &currentSigned); err != nil {
			return err
		}
		if currentOwner != j.Account || proof.SignedAt.Before(currentSigned) {
			return ErrSubscriptionConflict
		}
		if proof.Environment != j.Environment || proof.OriginalTransactionID != j.Original || proof.TransactionID == "" || proof.SignedAt.Before(j.SignedAt) {
			return ErrSubscriptionVerification
		}
		receipt, transaction, signed = at, proof.TransactionID, proof.SignedAt.UTC().Truncate(time.Microsecond)
	}
	result, err := tx.ExecContext(ctx, `UPDATE album_deletion_subscription_jobs SET acknowledged_at=?,verified_transaction_id=?,verified_signed_at=?,available_at=?,lease_token=NULL,lease_until=NULL
 WHERE environment=? AND original_transaction_id=? AND account_id=? AND request_id=? AND target_token=? AND lease_token=? AND lease_until>? AND acknowledged_at IS NULL`, receipt, transaction, signed, at.Add(accountDeletionRetry(j.Attempts)), j.Environment, j.Original, j.Account, j.Request, j.Target, j.Lease, at)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLease
	}
	return tx.Commit()
}

func (r *MySQLUploads) authorizeSubscriptionBinding(ctx context.Context, j subscriptionBindingJob, at time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.lockSubscriptionBinding(ctx, tx, j); err != nil {
		return err
	}
	if err = lockSubscriptionBindingLease(ctx, tx, j, at); err != nil {
		return err
	}
	return tx.Commit()
}

func lockSubscriptionBindingLease(ctx context.Context, tx *sql.Tx, j subscriptionBindingJob, at time.Time) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM album_deletion_subscription_jobs WHERE environment=? AND original_transaction_id=? AND account_id=? AND request_id=? AND target_token=? AND lease_token=? AND lease_until>? AND acknowledged_at IS NULL FOR UPDATE`, j.Environment, j.Original, j.Account, j.Request, j.Target, j.Lease, at).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLease
	}
	return err
}

// Only verified current statuses reach this path. Existing receipts are left
// intact; otherwise overlapping provider responses would endlessly requeue them.
func (r *MySQLUploads) discoverSubscriptionBindings(ctx context.Context, j subscriptionBindingJob, proofs []VerifiedAlbumSubscription, at time.Time) error {
	if len(proofs) == 0 || len(proofs) > 128 {
		return ErrSubscriptionVerification
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = r.lockSubscriptionBinding(ctx, tx, j); err != nil {
		return err
	}
	if err = lockSubscriptionBindingLease(ctx, tx, j, at); err != nil {
		return err
	}
	account, err := r.cloudAccountInTransaction(ctx, tx, j.Account, at)
	if err != nil {
		return err
	}
	for _, v := range proofs {
		if v.Environment != j.Environment || v.OriginalTransactionID == j.Original {
			return ErrSubscriptionVerification
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT owner_id FROM album_cloud_subscriptions WHERE environment=? AND original_transaction_id=? FOR UPDATE`, v.Environment, v.OriginalTransactionID).Scan(&owner)
		if err == nil {
			if owner != j.Account {
				return ErrSubscriptionBinding
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		account, err = r.applySubscriptionInTransaction(ctx, tx, j.Account, account.Revision, v, at)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *AlbumSubscriptionService) observeSubscriptionBinding(ctx context.Context, r *MySQLUploads, j subscriptionBindingJob) (VerifiedAlbumSubscription, bool, error) {
	states, err := s.resolver.client.AllSubscriptionStatuses(ctx, j.Transaction)
	if err != nil {
		return VerifiedAlbumSubscription{}, false, ErrSubscriptionUnavailable
	}
	proof, err := s.resolver.resolveStatuses(j.Account, j.Original, states, j.Target)
	if err != nil {
		return proof, false, err
	}
	if proof.SignedAt.Before(j.SignedAt) {
		return proof, false, ErrSubscriptionConflict
	}
	bound := true
	found := false
	related := map[string][]appstore.SubscriptionStatus{}
	owned := map[string]bool{}
	for _, state := range states {
		t, e := s.resolver.verifier.VerifyTransaction(state.SignedTransaction)
		if e != nil {
			return proof, false, ErrSubscriptionVerification
		}
		if t.OriginalTransactionID != j.Original {
			related[t.OriginalTransactionID] = append(related[t.OriginalTransactionID], state)
			if transactionBelongsToAccount(t, j.Account, j.Environment, j.Target) {
				owned[t.OriginalTransactionID] = true
			}
			continue
		}
		if t.IsUpgraded {
			continue
		}
		found = true
		if t.AppAccountToken != j.Target {
			bound = false
		}
	}
	if !found {
		return proof, false, ErrSubscriptionVerification
	}
	if len(owned) > 128 {
		return proof, false, ErrSubscriptionUnavailable
	}
	// Resolve each complete chain before saving any discovery or changing Apple.
	keys := make([]string, 0, len(owned))
	for original := range owned {
		keys = append(keys, original)
	}
	sort.Strings(keys)
	proofs := make([]VerifiedAlbumSubscription, 0, len(keys))
	for _, original := range keys {
		v, err := s.resolver.resolveStatuses(j.Account, original, related[original], j.Target)
		if err != nil {
			return proof, false, err
		}
		proofs = append(proofs, v)
	}
	if len(proofs) > 0 {
		if err := r.discoverSubscriptionBindings(ctx, j, proofs, s.now()); err != nil {
			return proof, false, err
		}
	}
	return proof, bound, nil
}

func (s *AlbumSubscriptionService) runSubscriptionBindingBatch(ctx context.Context, r *MySQLUploads, limit int) (subscriptionBindingBatch, error) {
	var b subscriptionBindingBatch
	if s == nil || s.resolver == nil || r == nil || r.billingEnvironment != s.resolver.environment || limit < 1 || limit > 100 {
		return b, ErrSubscriptionUnavailable
	}
	setter, ok := s.resolver.client.(subscriptionTokenSetter)
	if !ok {
		return b, ErrSubscriptionUnavailable
	}
	at := s.now().UTC().Truncate(time.Microsecond)
	rows, err := r.db.QueryContext(ctx, `SELECT account_id,request_id,environment,original_transaction_id,target_token FROM album_deletion_subscription_jobs WHERE environment=? AND acknowledged_at IS NULL AND available_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY available_at,original_transaction_id LIMIT ?`, r.billingEnvironment, at, at, limit)
	if err != nil {
		return b, err
	}
	var jobs []subscriptionBindingJob
	for rows.Next() {
		var j subscriptionBindingJob
		if err = rows.Scan(&j.Account, &j.Request, &j.Environment, &j.Original, &j.Target); err != nil {
			rows.Close()
			return b, err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	for _, j := range jobs {
		if err = ctx.Err(); err != nil {
			return b, err
		}
		j, err = r.claimSubscriptionBinding(ctx, j, s.now())
		if errors.Is(err, ErrLease) {
			b.Contended++
			continue
		}
		if err != nil {
			return b, err
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		proof, bound, e := s.observeSubscriptionBinding(call, r, j)
		if e == nil && !bound {
			e = r.authorizeSubscriptionBinding(call, j, s.now())
			if e == nil {
				e = setter.SetAppAccountToken(call, j.Original, j.Target)
			}
			if e == nil {
				proof, bound, e = s.observeSubscriptionBinding(call, r, j)
			}
		}
		var completion *bindingCompletionProof
		if e == nil && bound {
			completion, e = s.verifyBindingCompletion(call, r, j)
		}
		cancel()
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		var confirmed *VerifiedAlbumSubscription
		if e == nil && bound {
			confirmed = &proof
		}
		err = r.finishSubscriptionBinding(ctx, j, confirmed, completion, s.now())
		if errors.Is(err, ErrLease) {
			b.Contended++
			continue
		}
		if err != nil {
			return b, err
		}
		if confirmed != nil {
			b.Acknowledged++
		} else {
			b.Deferred++
		}
	}
	return b, nil
}

var _ subscriptionTokenSetter = (*appstore.APIClient)(nil)
