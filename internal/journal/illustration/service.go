package illustration

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/lifecycle"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

var ErrNotFound = errors.New("illustration task not found")

type Job struct {
	Task          Task
	Input         *Input  `json:",omitempty"`
	Result        *Result `json:",omitempty"`
	ConfigVersion string
	Problem       string
	LeaseUntil    time.Time
}

type Store interface {
	Create(context.Context, Job) (Job, error)
	Get(context.Context, string, string) (Job, error)
	Save(context.Context, Job, uint64) error
	Pending(context.Context, time.Time, int) ([]Task, error)
}

// Service uses durable compare-and-swap claims so overlapping release processes
// cannot generate the same image twice. An ambiguous dispatch is never retried.
type Service struct {
	Store     Store
	Generator ImageGenerator
	Prompts   *promptconfig.Cache
	Authorize func(context.Context, string) bool
	Now       func() time.Time
}

func (s *Service) Create(ctx context.Context, owner string, confirmation Confirmation, reference []byte) (Job, error) {
	task, err := NewTask("journal", owner, s.Generator.Model(), confirmation, reference, s.Now())
	if err != nil {
		return Job{}, err
	}
	version := "seed"
	if s.Prompts != nil {
		revision, err := s.Prompts.Current("journal")
		if err != nil {
			return Job{}, err
		}
		ctx = promptconfig.WithRevision(ctx, revision)
		version = revision.ID
	}
	prompt := confirmation.Brief.Compile(ctx)
	if len([]rune(prompt)) > 20000 {
		return Job{}, ErrInput
	}
	return s.Store.Create(ctx, Job{Task: task, Input: &Input{Prompt: prompt, Reference: reference}, ConfigVersion: version})
}

func (s *Service) Get(ctx context.Context, owner, id string) (Job, error) {
	job, err := s.Store.Get(ctx, owner, id)
	if err != nil {
		return Job{}, err
	}
	if !s.Now().Before(job.Task.ExpiresAt) && (job.Input != nil || job.Result != nil || job.Task.State != TaskCancelled) {
		previous := job.Task.Revision
		if err := job.Task.Cancel(previous); err != nil {
			return Job{}, err
		}
		job.Input, job.Result = nil, nil
		if err := s.Store.Save(ctx, job, previous); err != nil {
			return Job{}, err
		}
	}
	return job, nil
}

func (s *Service) Cancel(ctx context.Context, owner, id string, revision uint64) (Job, error) {
	job, err := s.Store.Get(ctx, owner, id)
	if err != nil {
		return Job{}, err
	}
	previous := job.Task.Revision
	if err = job.Task.Cancel(revision); err != nil {
		return Job{}, err
	}
	if job.Task.Revision == previous {
		return job, nil
	}
	job.Input, job.Result = nil, nil
	return job, s.Store.Save(ctx, job, previous)
}

func (s *Service) Run(ctx context.Context) {
	for lifecycle.Wait(ctx) {
		jobs, err := s.Store.Pending(ctx, s.Now(), 16)
		if err != nil && ctx.Err() == nil {
			slog.Warn("Journal image queue unavailable")
		}
		for _, job := range jobs {
			done, admitted := lifecycle.Begin(ctx)
			if !admitted {
				break
			}
			err := s.process(ctx, job.OwnerID, job.ID)
			done()
			if err != nil && !errors.Is(err, ErrTaskConflict) && ctx.Err() == nil {
				slog.Warn("Journal image task settlement unavailable")
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) process(ctx context.Context, owner, id string) error {
	job, err := s.Get(ctx, owner, id)
	if err != nil {
		return err
	}
	now := s.Now()
	if job.Task.State == TaskRunning && !now.Before(job.LeaseUntil) {
		previous := job.Task.Revision
		job.Task.State = TaskUncertain
		job.Task.Revision++
		job.Input = nil
		return s.Store.Save(ctx, job, previous)
	}
	if job.Task.State != TaskQueued || job.Input == nil {
		return nil
	}
	if s.Authorize == nil || !s.Authorize(ctx, owner) {
		// A temporarily unavailable entitlement must not monopolize the first queue page.
		previous := job.Task.Revision
		job.Task.Revision++
		job.LeaseUntil = now.Add(30 * time.Second)
		return s.Store.Save(ctx, job, previous)
	}
	if job.Task.Model != s.Generator.Model() {
		_, err := s.Cancel(ctx, owner, id, job.Task.Revision)
		return err
	}
	previous := job.Task.Revision
	if err := job.Task.Start(previous, now); err != nil {
		return err
	}
	job.LeaseUntil = now.Add(5 * time.Minute)
	if err := s.Store.Save(ctx, job, previous); err != nil {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Minute)
	var result Result
	var failure error
	if !s.Authorize(call, owner) {
		failure = NotDispatched{Cause: errors.New("image authorization revoked")}
	} else {
		result, failure = s.Generator.Generate(call, *job.Input)
	}
	cancel()
	// Cancellation of HTTP/process work must not prevent recording a known outcome.
	settle, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	for range 4 {
		current, err := s.Store.Get(settle, owner, id)
		if err != nil {
			return err
		}
		previous = current.Task.Revision
		current.Input = nil
		var notDispatched NotDispatched
		if errors.As(failure, &notDispatched) {
			err = current.Task.AbortBeforeDispatch()
		} else {
			var output *Result
			if failure == nil {
				output = &result
			}
			var deliver bool
			deliver, err = current.Task.RecordOutcome(previous, output, failure, s.Now())
			if deliver {
				current.Result = output
			}
		}
		if err != nil {
			return err
		}
		if failure != nil {
			current.Problem = FailureProblem(failure)
		}
		err = s.Store.Save(settle, current, previous)
		if !errors.Is(err, ErrTaskConflict) {
			return err
		}
	}
	return ErrTaskConflict
}

func FailureProblem(failure error) string {
	if errors.Is(failure, costcontrol.ErrBudgetExceeded) || errors.Is(failure, costcontrol.ErrConcurrencyExceeded) {
		return "quota"
	}
	var rejected Rejected
	if errors.As(failure, &rejected) {
		switch rejected.Status {
		case 401, 403, 404:
			return "unavailable"
		case 429:
			return "quota"
		default:
			return "rejected"
		}
	}
	if errors.Is(failure, ErrResult) {
		return "invalidImage"
	}
	return "network"
}
