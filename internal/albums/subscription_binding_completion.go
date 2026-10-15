package albums

import (
	"context"
	"database/sql"
	"time"

	"github.com/tellyouwhat/backend/internal/appstore"
)

type bindingCompletionAnchor struct {
	Original, Transaction string
	SignedAt              time.Time
}
type bindingCompletionProof struct {
	Revision  int64
	Anchors   []bindingCompletionAnchor
	CheckedAt time.Time
}

// Called with account/storage locks held. A missing receipt is never an empty
// queue. The last acknowledgement must close the same complete revision.
func (r *MySQLUploads) bindingCompletionSnapshot(ctx context.Context, tx *sql.Tx, j subscriptionBindingJob) (bindingCompletionProof, bool, error) {
	var p bindingCompletionProof
	if err := tx.QueryRowContext(ctx, `SELECT subscription_revision FROM album_storage_accounts WHERE owner_id=?`, j.Account).Scan(&p.Revision); err != nil {
		return p, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.environment,c.original_transaction_id,c.transaction_id,c.signed_at,b.account_id,b.request_id,b.target_token,b.acknowledged_at,b.verified_signed_at
 FROM album_cloud_subscriptions c LEFT JOIN album_deletion_subscription_jobs b ON b.environment=c.environment AND b.original_transaction_id=c.original_transaction_id
 WHERE c.owner_id=? ORDER BY c.environment,c.original_transaction_id LIMIT 129 FOR UPDATE`, j.Account)
	if err != nil {
		return p, false, err
	}
	defer rows.Close()
	ready, found := true, false
	for rows.Next() {
		var a bindingCompletionAnchor
		var env string
		var owner, request, target sql.NullString
		var ack, signed sql.NullTime
		if err = rows.Scan(&env, &a.Original, &a.Transaction, &a.SignedAt, &owner, &request, &target, &ack, &signed); err != nil {
			return p, false, err
		}
		if env != j.Environment || owner.String != j.Account || request.String != j.Request || target.String != j.Target {
			return p, false, ErrSubscriptionBinding
		}
		if a.Original == j.Original {
			found = true
		} else if !ack.Valid || !signed.Valid || signed.Time.Before(a.SignedAt) {
			ready = false
		}
		p.Anchors = append(p.Anchors, a)
	}
	if err = rows.Err(); err != nil {
		return p, false, err
	}
	if !found || len(p.Anchors) > 128 {
		return p, false, ErrSubscriptionUnavailable
	}
	return p, ready, nil
}

func (s *AlbumSubscriptionService) verifyBindingCompletion(ctx context.Context, r *MySQLUploads, j subscriptionBindingJob) (*bindingCompletionProof, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = r.lockSubscriptionBinding(ctx, tx, j); err != nil {
		return nil, err
	}
	if err = lockSubscriptionBindingLease(ctx, tx, j, s.now()); err != nil {
		return nil, err
	}
	p, ready, err := r.bindingCompletionSnapshot(ctx, tx, j)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if !ready {
		return nil, nil
	}
	known := map[string]bool{}
	for _, a := range p.Anchors {
		known[a.Original] = true
	}
	byChain := map[string][]appstore.SubscriptionStatus{}
	related := map[string]bool{}
	var discoveries []VerifiedAlbumSubscription
	for _, a := range p.Anchors {
		states, err := s.resolver.client.AllSubscriptionStatuses(ctx, a.Transaction)
		if err != nil {
			return nil, ErrSubscriptionUnavailable
		}
		for _, state := range states {
			t, e := s.resolver.verifier.VerifyTransaction(state.SignedTransaction)
			if e != nil {
				return nil, ErrSubscriptionVerification
			}
			byChain[t.OriginalTransactionID] = append(byChain[t.OriginalTransactionID], state)
			if known[t.OriginalTransactionID] || transactionBelongsToAccount(t, j.Account, j.Environment, j.Target) {
				related[t.OriginalTransactionID] = true
			}
		}
		// Each requested chain must actually be present, even if another response
		// happened to include it earlier.
		if _, err = s.resolver.resolveStatuses(j.Account, a.Original, states, j.Target); err != nil {
			return nil, err
		}
	}
	if len(related) > 128 {
		return nil, ErrSubscriptionUnavailable
	}
	for original := range related {
		states := byChain[original]
		if !known[original] {
			v, err := s.resolver.resolveStatuses(j.Account, original, states, j.Target)
			if err != nil {
				return nil, err
			}
			discoveries = append(discoveries, v)
		}
	}
	if len(discoveries) > 0 {
		if err = r.discoverSubscriptionBindings(ctx, j, discoveries, s.now()); err != nil {
			return nil, err
		}
		return nil, ErrSubscriptionConflict
	}
	for _, a := range p.Anchors {
		states := byChain[a.Original]
		v, err := s.resolver.resolveStatuses(j.Account, a.Original, states, j.Target)
		if err != nil {
			return nil, err
		}
		if v.SignedAt.Before(a.SignedAt) {
			return nil, ErrSubscriptionConflict
		}
		for _, state := range states {
			t, err := s.resolver.verifier.VerifyTransaction(state.SignedTransaction)
			if err != nil {
				return nil, ErrSubscriptionVerification
			}
			if !t.IsUpgraded && t.AppAccountToken != j.Target {
				return nil, ErrSubscriptionBinding
			}
		}
	}
	p.CheckedAt = s.now().UTC().Truncate(time.Microsecond)
	return &p, nil
}

func matchingBindingCompletion(current bindingCompletionProof, verified *bindingCompletionProof, at time.Time) bool {
	if verified == nil || verified.CheckedAt.IsZero() || at.Before(verified.CheckedAt) || !at.Before(verified.CheckedAt.Add(time.Minute)) || current.Revision != verified.Revision || len(current.Anchors) != len(verified.Anchors) {
		return false
	}
	for i, a := range current.Anchors {
		b := verified.Anchors[i]
		if a.Original != b.Original || a.Transaction != b.Transaction || !a.SignedAt.Equal(b.SignedAt) {
			return false
		}
	}
	return true
}
