package albums

import (
	"context"
	"errors"
	"testing"
	"time"
)

type reconciliationQueueStub struct {
	jobs     []ReconciliationJob
	claimErr error
	finished int
}

func (q *reconciliationQueueStub) PendingReconciliations(context.Context, time.Time, int) ([]ReconciliationJob, error) {
	return q.jobs, nil
}
func (q *reconciliationQueueStub) ClaimReconciliation(_ context.Context, j ReconciliationJob, _ time.Time) (ReconciliationJob, error) {
	return j, q.claimErr
}
func (q *reconciliationQueueStub) FinishReconciliation(ctx context.Context, _ ReconciliationJob, _ time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing finish deadline")
	}
	q.finished++
	return nil
}
func TestReconciliationWorkerClaimsBeforeReadingAndFinishesOnCancel(t *testing.T) {
	for _, scenario := range []string{"success", "contended", "cancel", "failed", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q := &reconciliationQueueStub{jobs: []ReconciliationJob{{OperationID: "expected"}}}
			if scenario == "contended" {
				q.claimErr = ErrLease
			}
			if scenario == "oversized" {
				q.jobs = append(q.jobs, q.jobs[0])
			}
			reads := 0
			w := &ReconciliationWorker{queue: q, batchSize: 1, now: time.Now, reconcile: func(ctx context.Context, j ReconciliationJob) error {
				reads++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 15*time.Minute {
					t.Fatal("read unbounded")
				}
				if j.OperationID != "expected" {
					t.Fatal("wrong claimed operation")
				}
				if scenario == "cancel" {
					cancel()
					return ctx.Err()
				}
				if scenario == "failed" {
					return ErrCOSOperation
				}
				return nil
			}}
			result, err := w.RunOnce(ctx)
			switch scenario {
			case "success":
				if err != nil || result.Assembled != 1 || q.finished != 1 {
					t.Fatal(result, err)
				}
			case "contended":
				if err != nil || reads != 0 || q.finished != 0 || result.Contended != 1 {
					t.Fatal(result, err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || q.finished != 1 {
					t.Fatal("cancel lost finish", err)
				}
			case "failed":
				if err != nil || q.finished != 1 || result.Failed != 1 {
					t.Fatal(result, err)
				}
			case "oversized":
				if err == nil || reads != 0 {
					t.Fatal("unbounded queue accepted")
				}
			}
		})
	}
}

func TestReconciliationPollStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &ReconciliationWorker{queue: &reconciliationQueueStub{}, batchSize: 1, now: time.Now}
	observations := 0
	err := w.Run(ctx, time.Second, func(_ ReconciliationBatch, _ bool) { observations++; cancel() })
	if !errors.Is(err, context.Canceled) || observations != 1 {
		t.Fatal("poll did not stop", err, observations)
	}
	if err := w.Run(context.Background(), 0, nil); err == nil {
		t.Fatal("busy polling accepted")
	}
}
