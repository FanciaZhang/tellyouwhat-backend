package albums

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

func (s *AlbumSubscriptionService) refreshOne(ctx context.Context, repository *MySQLUploads) (bool, error) {
	if s == nil || s.resolver == nil || repository == nil || repository.billingEnvironment != s.resolver.environment {
		return false, ErrSubscriptionUnavailable
	}
	claim, found, err := repository.claimSubscriptionRefresh(ctx, s.now().UTC())
	if err != nil || !found {
		return false, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	before, err := repository.CloudAccount(requestCtx, claim.Owner, s.now().UTC())
	var v VerifiedAlbumSubscription
	if err == nil {
		v, err = s.resolver.resolveCurrent(requestCtx, claim.Owner, claim.Original, claim.Transaction, before.AppAccountToken)
	}
	if err == nil {
		err = repository.finishSubscriptionRefresh(requestCtx, claim, v, s.now().UTC())
	}
	if err != nil {
		// Failures change only retry metadata. They never renew freshness, infer
		// expiry from an empty API response, or delete the user's cloud contents.
		if errors.Is(err, ErrLease) {
			return true, nil
		}
		if retryErr := repository.retrySubscriptionRefresh(ctx, claim, s.now().UTC()); retryErr != nil && !errors.Is(retryErr, ErrLease) {
			return true, retryErr
		}
	}
	return true, err
}

func (g *Gateway) startSubscriptionRefresh(service *AlbumSubscriptionService, repository *MySQLUploads) {
	if service == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.subscriptionCancel = cancel
	g.startSubscriptionWorkers(ctx, service, repository)
	if service.sandbox != nil {
		// The dual-environment factory constructs this database-scoped child.
		g.startSubscriptionWorkers(ctx, service.sandbox, service.sandbox.repository.(*MySQLUploads))
	}
}

func (g *Gateway) startSubscriptionWorkers(ctx context.Context, service *AlbumSubscriptionService, repository *MySQLUploads) {
	if service.rebindingEnabled {
		g.subscriptionWorkers.Add(1)
		go func() {
			defer g.subscriptionWorkers.Done()
			for ctx.Err() == nil {
				batch, err := service.runSubscriptionBindingBatch(ctx, repository, 10)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					slog.Warn("album subscription rebinding queue unavailable")
				}
				if batch.Acknowledged+batch.Deferred+batch.Contended > 0 {
					slog.Info("album subscription rebinding batch", "acknowledged", batch.Acknowledged, "deferred", batch.Deferred, "contended", batch.Contended)
				}
				if err == nil && service.recordErasureEnabled {
					erased, eraseErr := repository.runAccountRecordErasureBatch(ctx, 10, service.now)
					if eraseErr != nil && ctx.Err() == nil {
						slog.Warn("album account record erasure queue unavailable")
					}
					if erased > 0 {
						slog.Info("album account records erased", "accounts", erased)
					}
					retired, retireErr := repository.runIdentityRetirementBatch(ctx, 10, service.now)
					if retireErr != nil && ctx.Err() == nil {
						slog.Warn("album identity retirement queue unavailable")
					}
					if retired > 0 {
						slog.Info("album identity fences retired", "identities", retired)
					}
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
	// Bounded concurrency. Database leases fence multiple API instances and
	// restart recovery; only this API holds the App Store private key.
	for range 4 {
		g.subscriptionWorkers.Add(1)
		go func() {
			defer g.subscriptionWorkers.Done()
			for ctx.Err() == nil {
				found, err := service.refreshOne(ctx, repository)
				if found && err == nil {
					continue
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
}
