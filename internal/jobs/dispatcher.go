package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

type Dispatcher interface {
	Dispatch(context.Context, string) error
}

type DispatchItem struct {
	JobID        string
	Attempts     int
	AvailableAt  time.Time
	ClaimedUntil time.Time
}

type OutboxStore interface {
	ClaimDispatches(context.Context, time.Time, int) ([]DispatchItem, error)
	CompleteDispatch(context.Context, string, time.Time) error
	RetryDispatch(context.Context, string, time.Time, string) error
}

type DurableQueueDispatcher struct{}

// Dispatch is intentionally a no-op: production JobStore.CreateOrGet writes
// the durable outbox entry in the same transaction as the job.
func (DurableQueueDispatcher) Dispatch(context.Context, string) error { return nil }

type OutboxPump struct {
	store      OutboxStore
	dispatcher Dispatcher
	now        func() time.Time
	interval   time.Duration
}

func NewOutboxPump(store OutboxStore, dispatcher Dispatcher, now func() time.Time) *OutboxPump {
	if now == nil {
		now = time.Now
	}
	return &OutboxPump{store: store, dispatcher: dispatcher, now: now, interval: time.Second}
}

func (pump *OutboxPump) Run(ctx context.Context) error {
	if pump == nil || pump.store == nil || pump.dispatcher == nil {
		return errors.New("outbox pump is not configured")
	}
	ticker := time.NewTicker(pump.interval)
	defer ticker.Stop()
	for {
		_ = pump.drain(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (pump *OutboxPump) drain(ctx context.Context) error {
	items, err := pump.store.ClaimDispatches(ctx, pump.now(), 20)
	if err != nil {
		return err
	}
	var wait sync.WaitGroup
	for _, item := range items {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if dispatchErr := pump.dispatcher.Dispatch(ctx, item.JobID); dispatchErr != nil {
				_ = pump.store.RetryDispatch(persistContext, item.JobID, pump.now(), "worker dispatch rejected")
				return
			}
			_ = pump.store.CompleteDispatch(persistContext, item.JobID, pump.now())
		}()
	}
	wait.Wait()
	return nil
}

type HTTPDispatcher struct {
	url    string
	secret string
	client *http.Client
}

func NewHTTPDispatcher(url, secret string, client *http.Client) *HTTPDispatcher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPDispatcher{url: url, secret: secret, client: client}
}

func (dispatcher *HTTPDispatcher) Dispatch(ctx context.Context, jobID string) error {
	if dispatcher == nil || dispatcher.url == "" || dispatcher.secret == "" || jobID == "" {
		return errors.New("worker dispatcher is not configured")
	}
	body, err := json.Marshal(map[string]string{"jobID": jobID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, dispatcher.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Health-Worker-Secret", dispatcher.secret)
	response, err := dispatcher.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("worker dispatch was rejected")
	}
	return nil
}

type LocalDispatcher struct{ worker *Worker }

func NewLocalDispatcher(worker *Worker) *LocalDispatcher {
	return &LocalDispatcher{worker: worker}
}

func (dispatcher *LocalDispatcher) Dispatch(ctx context.Context, jobID string) error {
	if dispatcher == nil || dispatcher.worker == nil {
		return errors.New("local worker is unavailable")
	}
	go func() {
		workerContext := context.WithoutCancel(ctx)
		for attempt := 0; attempt < maximumAttempts; attempt++ {
			if err := dispatcher.worker.Process(workerContext, jobID); err == nil {
				return
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			<-timer.C
		}
	}()
	return nil
}

