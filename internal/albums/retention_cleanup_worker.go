package albums

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type retentionCleanupBatch struct{ Advanced, Settled, Collections, Deferred, Contended int }
type retentionCleanupWorker struct {
	repository    *MySQLUploads
	subscriptions *AlbumSubscriptionService
	storage       objectPurgeStorage
	batchSize     int
	now           func() time.Time
	trashEnabled  bool
}

func (w *retentionCleanupWorker) check(ctx context.Context) error {
	if w == nil || w.repository == nil || w.repository.db == nil || w.storage == nil || w.now == nil || w.batchSize < 1 || w.batchSize > 100 ||
		w.repository.developmentCloud != nil || (w.repository.billingEnvironment != "Production" && w.repository.billingEnvironment != "Sandbox") ||
		(w.subscriptions == nil && !w.trashEnabled) {
		return ErrSubscriptionUnavailable
	}
	if w.subscriptions != nil && (w.subscriptions.resolver == nil || w.repository.billingEnvironment != w.subscriptions.resolver.environment ||
		(w.repository.billingEnvironment == "Sandbox" && w.repository.productionSandboxQuota > 0)) {
		return ErrSubscriptionUnavailable
	}
	rows, err := w.repository.db.QueryContext(ctx, `SELECT retention_cleanup_after,retention_cleanup_token,retention_cleanup_until,retention_cleanup_cursor FROM album_storage_accounts LIMIT 0`)
	if err != nil {
		return err
	}
	return rows.Close()
}

func (w *retentionCleanupWorker) advanceAsset(ctx context.Context, owner, asset string, b *retentionCleanupBatch) error {
	p, err := w.repository.objectPurge(ctx, owner, asset)
	if err != nil {
		return err
	}
	if p.State != "objects_removed" {
		if err := w.repository.runObjectPurgeStep(ctx, owner, asset, w.storage, w.now); err != nil {
			return err
		}
		b.Advanced++
		p, err = w.repository.objectPurge(ctx, owner, asset)
		if err != nil {
			return err
		}
	}
	if p.State == "objects_removed" {
		if _, err := w.repository.settleObjectPurge(ctx, owner, asset, w.storage, w.now); err != nil {
			return err
		}
		b.Settled++
	}
	return nil
}

func (w *retentionCleanupWorker) advance(ctx context.Context, c retentionCleanupClaim, b *retentionCleanupBatch) (string, error) {
	// An already granted delete may have reached COS. Renewal must not make that
	// asset available again; finish its exact persisted plan without new grants.
	mode := mediaCleanupMode{trash: w.trashEnabled, retention: w.subscriptions != nil}
	started, err := w.repository.retentionCleanupAssets(ctx, c.owner, "", true, w.batchSize, w.now().UTC(), mode)
	if err != nil {
		return c.cursor, err
	}
	var deferred error
	for _, asset := range started {
		if err := w.advanceAsset(ctx, c.owner, asset, b); err != nil {
			deferred = err
		}
		if ctx.Err() != nil {
			return c.cursor, ctx.Err()
		}
	}
	var proof *expiredRetentionProof
	if mode.retention {
		account, err := w.repository.CloudAccount(ctx, c.owner, w.now().UTC())
		if err != nil {
			deferred = err
		} else if account.State == "retention_due" {
			verified, err := w.subscriptions.verifyExpiredRetention(ctx, w.repository, c.owner)
			if err != nil {
				deferred = err
			} else {
				proof = &verified
				count, err := w.repository.expireRetentionCollections(ctx, verified, 100, w.now)
				b.Collections += count
				if err != nil {
					deferred = err
				}
			}
		}
	}
	mode.retention = proof != nil
	if !mode.trash && !mode.retention {
		return c.cursor, deferred
	}
	assets, err := w.repository.retentionCleanupAssets(ctx, c.owner, c.cursor, false, w.batchSize, w.now().UTC(), mode)
	if err != nil {
		return c.cursor, err
	}
	cursor := ""
	for _, asset := range assets {
		// Persist the page boundary even for a blocked asset, so it cannot starve
		// later media. The next complete scan revisits unresolved entries.
		cursor = asset
		if _, err := w.repository.prepareScheduledObjectPurge(ctx, c.owner, asset, proof, w.now().UTC()); err != nil {
			deferred = err
		} else if err := w.advanceAsset(ctx, c.owner, asset, b); err != nil {
			deferred = err
		}
		if ctx.Err() != nil {
			return cursor, ctx.Err()
		}
	}
	return cursor, deferred
}

func (w *retentionCleanupWorker) runOnce(ctx context.Context) (retentionCleanupBatch, error) {
	var b retentionCleanupBatch
	if err := w.check(ctx); err != nil {
		return b, err
	}
	owners, err := w.repository.retentionCleanupCandidates(ctx, w.now().UTC(), w.batchSize, mediaCleanupMode{trash: w.trashEnabled, retention: w.subscriptions != nil})
	if err != nil {
		return b, err
	}
	for _, owner := range owners {
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		c, err := w.repository.claimRetentionCleanup(ctx, owner, w.now)
		if errors.Is(err, ErrLease) {
			b.Contended++
			continue
		}
		if err != nil {
			return b, err
		}
		operation, cancel := context.WithTimeout(ctx, time.Minute)
		cursor, stepErr := w.advance(operation, c, &b)
		cancel()
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		if err := w.repository.finishRetentionCleanup(ctx, c, cursor, stepErr != nil, w.now().UTC()); errors.Is(err, ErrLease) {
			b.Contended++
		} else if err != nil {
			return b, err
		}
		if stepErr != nil {
			b.Deferred++
		}
	}
	return b, nil
}

func (g *Gateway) startRetentionCleanup(parent context.Context, w *retentionCleanupWorker) {
	ctx, cancel := context.WithCancel(parent)
	g.retentionCleanupCancel = cancel
	g.subscriptionWorkers.Add(1)
	go func() {
		defer g.subscriptionWorkers.Done()
		for ctx.Err() == nil {
			if !g.runBackgroundBatch(ctx, func() {
				b, err := w.runOnce(ctx)
				if err != nil && ctx.Err() == nil {
					slog.Warn("album media cleanup unavailable")
				}
				if b != (retentionCleanupBatch{}) {
					slog.Info("album media cleanup batch", "advanced", b.Advanced, "settled", b.Settled, "collections", b.Collections, "deferred", b.Deferred, "contended", b.Contended)
				}
			}) {
				return
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
