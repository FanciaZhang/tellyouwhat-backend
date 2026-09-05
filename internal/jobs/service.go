package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	providerapi "github.com/tellyouwhat/backend/internal/provider"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

var (
	ErrNotFound            = errors.New("job not found")
	ErrIdempotencyConflict = errors.New("job idempotency conflict")
	ErrJobNotClaimable     = errors.New("job is not claimable")
	ErrJobLeaseBusy        = errors.New("job processing lease is active")
)

const (
	claimLeaseDuration = 2 * time.Minute
	maximumAttempts    = 3
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

type Job struct {
	AppID              string
	ID                 string
	RequestID          string
	BodyDigest         string
	OwnerKeyID         string
	OwnerDeviceID      string
	OwnerTransactionID string
	Request            contracts.Request
	Status             Status
	Result             string
	InputTokens        int
	OutputTokens       int
	FailureCategory    string
	AttemptCount       int
	ClaimExpiresAt     time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ExpiresAt          time.Time
}

type Store interface {
	CreateOrGet(context.Context, Job) (Job, error)
	Get(context.Context, string) (Job, error)
	Claim(context.Context, string, time.Time) (Job, error)
	ExtendLease(context.Context, string, int, time.Time) error
	Succeed(context.Context, string, int, providerapi.Response, usage.Record, time.Time) error
	Fail(context.Context, string, int, string, time.Time) error
	RetryOrFail(context.Context, string, int, string, time.Time) (bool, error)
	Cancel(context.Context, string, time.Time) error
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

func (service *Service) Enqueue(
	ctx context.Context,
	principal attestation.Principal,
	request contracts.Request,
	bodyDigest string,
) (Job, error) {
	jobID, err := newJobID()
	if err != nil {
		return Job{}, err
	}
	return service.EnqueueWithID(ctx, principal, jobID, request, bodyDigest)
}

func (service *Service) EnqueueWithID(
	ctx context.Context,
	principal attestation.Principal,
	jobID string,
	request contracts.Request,
	bodyDigest string,
) (Job, error) {
	if service == nil || service.store == nil || principal.KeyID == "" || bodyDigest == "" {
		return Job{}, ErrIdempotencyConflict
	}
	if !contracts.ValidRequestID(jobID) {
		return Job{}, ErrIdempotencyConflict
	}
	now := service.now()
	job := Job{
		AppID:              principal.AppID,
		ID:                 jobID,
		RequestID:          request.RequestID,
		BodyDigest:         bodyDigest,
		OwnerKeyID:         principal.KeyID,
		OwnerDeviceID:      principal.DeviceID,
		OwnerTransactionID: principal.TransactionID,
		Request:            request,
		Status:             StatusQueued,
		CreatedAt:          now,
		UpdatedAt:          now,
		ExpiresAt:          now.Add(24 * time.Hour),
	}
	stored, err := service.store.CreateOrGet(ctx, job)
	if err != nil {
		return Job{}, err
	}
	if !service.now().Before(stored.ExpiresAt) {
		return Job{}, ErrIdempotencyConflict
	}
	return stored, nil
}

func (service *Service) Get(ctx context.Context, principal attestation.Principal, jobID string) (Job, error) {
	job, err := service.store.Get(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.OwnerKeyID != principal.KeyID || !service.now().Before(job.ExpiresAt) {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (service *Service) Cancel(ctx context.Context, principal attestation.Principal, jobID string) error {
	job, err := service.Get(ctx, principal, jobID)
	if err != nil {
		return err
	}
	return service.store.Cancel(ctx, job.ID, service.now())
}

type Worker struct {
	store      Store
	provider   providerapi.Client
	reconciler quota.TokenReconciler
}

func NewWorker(store Store, provider providerapi.Client, reconciler quota.TokenReconciler) *Worker {
	return &Worker{store: store, provider: provider, reconciler: reconciler}
}

func (worker *Worker) Process(ctx context.Context, jobID string) error {
	job, err := worker.store.Claim(ctx, jobID, time.Now())
	if err != nil {
		if errors.Is(err, ErrJobNotClaimable) {
			existing, getErr := worker.store.Get(ctx, jobID)
			if getErr == nil && (existing.Status == StatusSucceeded || existing.Status == StatusFailed || existing.Status == StatusCancelled) {
				return nil
			}
			return ErrJobLeaseBusy
		}
		return err
	}
	workContext, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go worker.heartbeat(workContext, job.ID, job.AttemptCount, cancelWork, stopHeartbeat, heartbeatDone)
	defer func() {
		close(stopHeartbeat)
		<-heartbeatDone
	}()
	response, err := worker.provider.Complete(workContext, job.Request)
	if err != nil {
		retry, persistErr := worker.store.RetryOrFail(ctx, job.ID, job.AttemptCount, "upstream", time.Now())
		if persistErr == nil && !retry {
			worker.reconcileQuota(ctx, job, providerapi.Response{})
		}
		return err
	}
	if err := contracts.ValidateResponse(job.Request, response.Content); err != nil {
		if err := worker.store.Fail(ctx, job.ID, job.AttemptCount, "contract", time.Now()); err == nil {
			worker.reconcileQuota(ctx, job, providerapi.Response{})
		}
		return errors.New("job result violates JSON contract")
	}
	transactionID := job.OwnerTransactionID
	if transactionID == "" {
		transactionID = job.OwnerKeyID
	}
	completedAt := time.Now()
	usageRecord := usage.Record{
		RequestID: job.RequestID, KeyID: job.OwnerKeyID, DeviceID: job.OwnerDeviceID,
		TransactionID: transactionID, Operation: job.Request.Operation,
		InputTokens: response.InputTokens, OutputTokens: response.OutputTokens, OccurredAt: completedAt,
	}
	if err := worker.store.Succeed(ctx, job.ID, job.AttemptCount, response, usageRecord, completedAt); err != nil {
		return err
	}
	if cleaner, ok := worker.provider.(providerapi.ManagedMediaCleaner); ok && len(job.Request.Media) > 0 {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleaner.CleanupManagedMedia(cleanupContext, job.Request.Media)
		cancel()
	}
	worker.reconcileQuota(ctx, job, response)
	return nil
}

func (worker *Worker) reconcileQuota(ctx context.Context, job Job, response providerapi.Response) {
	if worker.reconciler == nil {
		return
	}
	transactionID := job.OwnerTransactionID
	if transactionID == "" {
		transactionID = job.OwnerKeyID
	}
	reserved := contracts.ReservationTokens(job.Request)
	priorAttempts := reserved * (job.AttemptCount - 1)
	actualTokens := priorAttempts + reserved
	if tokens, known := response.KnownTokenTotal(); known && tokens <= math.MaxInt-priorAttempts {
		actualTokens = priorAttempts + tokens
	}
	// Prior attempts have unknown provider usage. Preserve a full reservation
	// for each of them, then use measured usage for the terminal attempt when
	// available. A durable terminal job must never be redispatched merely
	// because quota reconciliation failed.
	reconcileContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = worker.reconciler.Reconcile(
		reconcileContext,
		transactionID,
		quota.JobReservationID(job.OwnerKeyID, job.RequestID, job.BodyDigest),
		reserved,
		actualTokens,
		time.Now(),
	)
}

func (worker *Worker) heartbeat(
	ctx context.Context,
	jobID string,
	attempt int,
	cancelWork context.CancelFunc,
	stop <-chan struct{},
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case now := <-ticker.C:
			if err := worker.store.ExtendLease(ctx, jobID, attempt, now); err != nil {
				cancelWork()
				return
			}
		}
	}
}

func newJobID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
