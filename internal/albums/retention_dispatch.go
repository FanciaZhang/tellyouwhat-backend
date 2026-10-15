package albums

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type albumRetentionSender interface {
	sendRetention(context.Context, string, albumRetentionReminder, string) (albumAPNSResult, error)
}

type albumRetentionDispatcher struct {
	repository        *MySQLUploads
	subscriptions     *AlbumSubscriptionService
	registrations     *albumPushRegistrations
	sender            albumRetentionSender
	now               func() time.Time
	developmentSender albumRetentionSender
}

func (d *albumRetentionDispatcher) dispatchOne(ctx context.Context) (bool, error) {
	claim, found, err := d.repository.claimRetentionReminder(ctx, d.now().UTC())
	if err != nil || !found {
		return false, err
	}
	revision, err := d.subscriptions.verifyRetentionReminder(ctx, d.repository, claim)
	if err != nil || revision == 0 {
		retryErr := d.repository.finishRetentionReminder(ctx, claim, false, d.now().UTC())
		if retryErr != nil && !errors.Is(retryErr, ErrLease) {
			return true, retryErr
		}
		if errors.Is(err, ErrLease) {
			err = nil
		}
		return true, err
	}
	// Bound work under the one-minute parent lease; the next claim resumes receipts.
	for range 16 {
		if claim.Until.Sub(d.now().UTC()) < time.Second {
			break
		}
		delivery, found, nextErr := d.repository.nextRetentionDelivery(ctx, claim, revision, d.registrations.environments(), d.now().UTC())
		if nextErr != nil {
			err = nextErr
			break
		}
		if !found {
			break
		}
		store, selectErr := d.registrations.forEnvironment(delivery.Environment)
		if selectErr != nil {
			err = selectErr
			break
		}
		sender := d.sender
		if delivery.Environment != d.registrations.environment {
			sender = d.developmentSender
		}
		if sender == nil {
			err = errAlbumAPNS
			break
		}
		target, targetErr := store.target(ctx, claim.Owner, delivery.Registration.ID, delivery.Registration.Revision)
		if targetErr != nil {
			err = targetErr
			break
		}
		result, sendErr := sender.sendRetention(ctx, target.Token, claim, delivery.ID)
		// Graceful shutdown still saves a known response before closing the DB.
		receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		recordErr := d.repository.recordRetentionDelivery(receiptCtx, claim, delivery, result, d.now().UTC())
		if recordErr == nil && result.Status == "unregistered" {
			recordErr = store.invalidate(receiptCtx, target.Registration.ID, target.Registration.Revision, result.InvalidatedAt)
		}
		cancel()
		if recordErr != nil {
			err = recordErr
			break
		}
		if sendErr != nil {
			err = sendErr
		}
		if result.Status == "rejected" {
			err = errAlbumAPNS
		}
	}
	settleErr := d.repository.settleRetentionDispatch(ctx, claim, d.registrations.environments(), d.now().UTC())
	if settleErr != nil && !errors.Is(settleErr, ErrLease) {
		return true, settleErr
	}
	if errors.Is(err, ErrLease) {
		err = nil
	}
	return true, err
}

func (g *Gateway) startRetentionDispatch(d *albumRetentionDispatcher) {
	ctx, cancel := context.WithCancel(context.Background())
	g.retentionCancel = cancel
	g.subscriptionWorkers.Add(1)
	go func() {
		defer g.subscriptionWorkers.Done()
		for ctx.Err() == nil {
			found, err := d.dispatchOne(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Warn("album retention reminder deferred")
			}
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
