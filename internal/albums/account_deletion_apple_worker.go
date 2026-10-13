package albums

import (
	"context"
	"log/slog"
	"time"
)

// The API already owns the album login key and credential cipher. Media workers
// need neither. Durable database leases coordinate multiple API instances.
func (g *Gateway) startAppleDeletion(accounts *MySQLAccounts, exchanger *AppleCodeExchanger, batchSize int, interval time.Duration) error {
	if accounts == nil || exchanger == nil || batchSize < 1 || batchSize > 100 || interval < time.Second || interval > time.Minute {
		return ErrInvalidManifest
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.appleDeletionCancel = cancel
	g.subscriptionWorkers.Add(1)
	go func() {
		defer g.subscriptionWorkers.Done()
		for ctx.Err() == nil {
			batch, err := accounts.runAppleDeletionBatch(ctx, exchanger.revokeRefreshCredential, batchSize, time.Now)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				slog.Warn("album Apple deletion queue unavailable")
			}
			if batch.Acknowledged+batch.Deferred+batch.Contended > 0 {
				slog.Info("album Apple deletion batch", "acknowledged", batch.Acknowledged, "deferred", batch.Deferred, "contended", batch.Contended)
			}
			if err == nil {
				erased, eraseErr := accounts.runAccessErasureBatch(ctx, batchSize, time.Now)
				if ctx.Err() != nil {
					return
				}
				if eraseErr != nil {
					slog.Warn("album access erasure queue unavailable")
				}
				if erased > 0 {
					slog.Info("album access erasure batch", "accounts", erased)
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}
