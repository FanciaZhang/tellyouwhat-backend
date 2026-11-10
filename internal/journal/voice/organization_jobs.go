package voice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/lifecycle"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"github.com/tellyouwhat/backend/internal/quota"
)

var ErrOrganizationNotFound = errors.New("organization task not found")
var ErrOrganizationConflict = errors.New("organization task changed")
var ErrOrganizationExpired = errors.New("organization task expired")

type OrganizationJob struct {
	IP                                            string
	Principal                                     attestation.Principal
	ID, OwnerID, Digest, State, Problem           string
	Revision                                      uint64
	CreatedAt, ExpiresAt, AvailableAt, LeaseUntil time.Time
	Plan                                          *OrganizationPlan
	Progress                                      OrganizationState
	Policy                                        *promptconfig.Revision
}

type OrganizationStore interface {
	Create(context.Context, OrganizationJob) (OrganizationJob, error)
	Get(context.Context, string, string) (OrganizationJob, error)
	Save(context.Context, OrganizationJob, uint64) error
	Pending(context.Context, time.Time, int) ([][2]string, error)
	ForgetOwner(context.Context, string) error
}

type OrganizationService struct {
	Store     OrganizationStore
	Model     Rewriter
	Prompts   *promptconfig.Cache
	Authorize func(context.Context, string) bool
	Admit     func(context.Context, OrganizationJob, Snapshot) (func(RewriteResult, error), error)
	Usage     func(context.Context, string, string, RewriteResult)
	Now       func() time.Time
}

func (s *OrganizationService) Create(ctx context.Context, principal attestation.Principal, id, digest string, data []byte, ip string) (OrganizationJob, error) {
	owner := principal.KeyID
	hash := sha256.Sum256(data)
	if s.Store == nil || s.Model == nil || s.Now == nil || principal.AppID != "journal" || principal.DeviceID == "" || net.ParseIP(ip) == nil || strings.TrimSpace(owner) == "" || len(owner) > 512 || uuid.Validate(id) != nil || id != strings.ToLower(id) ||
		len(data) == 0 || len(data) > MaxOrganizationBytes || digest != hex.EncodeToString(hash[:]) {
		return OrganizationJob{}, ErrInvalid
	}
	if s.Authorize == nil || !s.Authorize(ctx, owner) {
		return OrganizationJob{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plan OrganizationPlan
	if err := decoder.Decode(&plan); err != nil {
		return OrganizationJob{}, ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF || plan.Validate() != nil {
		return OrganizationJob{}, ErrInvalid
	}
	now := s.Now()
	job := OrganizationJob{Principal: principal, IP: ip, ID: id, OwnerID: owner, Digest: digest, State: "queued", Revision: 1, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), AvailableAt: now, Plan: &plan}
	if s.Prompts != nil {
		policy, err := s.Prompts.Current("journal")
		if err != nil {
			return OrganizationJob{}, err
		}
		job.Policy = &policy
	}
	existing, err := s.Store.Create(ctx, job)
	if err == nil && !s.Now().Before(existing.ExpiresAt) {
		return OrganizationJob{}, ErrOrganizationExpired
	}
	return existing, err
}

func (s *OrganizationService) Get(ctx context.Context, owner, id string) (OrganizationJob, error) {
	job, err := s.Store.Get(ctx, owner, id)
	if err != nil {
		return job, err
	}
	if !s.Now().Before(job.ExpiresAt) && job.State != "expired" {
		previous := job.Revision
		job.Revision++
		job.State = "expired"
		job.Plan = nil
		job.Policy = nil
		job.Progress = OrganizationState{}
		if err := s.Store.Save(ctx, job, previous); err != nil {
			return OrganizationJob{}, err
		}
	}
	return job, nil
}

func (s *OrganizationService) Retry(ctx context.Context, owner, id string, revision uint64) (OrganizationJob, error) {
	job, err := s.Get(ctx, owner, id)
	if err != nil {
		return job, err
	}
	if job.Revision != revision || job.Plan == nil || (job.State != "failed" && job.State != "uncertain") {
		return OrganizationJob{}, ErrOrganizationConflict
	}
	if s.Authorize == nil || !s.Authorize(ctx, owner) {
		return OrganizationJob{}, ErrInvalid
	}
	previous := job.Revision
	job.Revision++
	job.State = "queued"
	job.Problem = ""
	job.LeaseUntil = time.Time{}
	job.AvailableAt = s.Now()
	return job, s.Store.Save(ctx, job, previous)
}

func (s *OrganizationService) Cancel(ctx context.Context, principal attestation.Principal, id string, revision uint64) (OrganizationJob, error) {
	owner := principal.KeyID
	if revision == 0 || s.Now == nil || principal.AppID != "journal" || owner == "" || uuid.Validate(id) != nil {
		return OrganizationJob{}, ErrInvalid
	}
	// A task ID never changes input; cancelling it includes newer worker progress.
	for range 4 {
		job, err := s.Get(ctx, owner, id)
		if errors.Is(err, ErrOrganizationNotFound) {
			// An in-flight create must not resurrect a task cancelled before its receipt.
			now := s.Now()
			job = OrganizationJob{Principal: principal, ID: id, OwnerID: owner, Digest: strings.Repeat("0", 64), State: "cancelled", Revision: 1, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), AvailableAt: now}
			job, err = s.Store.Create(ctx, job)
			if errors.Is(err, ErrOrganizationConflict) {
				continue
			}
			return job, err
		}
		if err != nil {
			return job, err
		}
		if job.State == "cancelled" || job.State == "expired" {
			return job, nil
		}
		previous := job.Revision
		job.Revision++
		job.State = "cancelled"
		job.Plan = nil
		job.Policy = nil
		job.Progress = OrganizationState{}
		job.LeaseUntil = time.Time{}
		err = s.Store.Save(ctx, job, previous)
		if !errors.Is(err, ErrOrganizationConflict) {
			return job, err
		}
	}
	return OrganizationJob{}, ErrOrganizationConflict
}

// One bounded batch per claim gives other recordings a turn between model calls.
func (s *OrganizationService) Process(ctx context.Context, owner, id string) error {
	job, err := s.Get(ctx, owner, id)
	if err != nil {
		return err
	}
	if job.State == "running" && !s.Now().Before(job.LeaseUntil) {
		previous := job.Revision
		job.Revision++
		job.State = "uncertain"
		job.Problem = "voice_rewrite_interrupted"
		return s.Store.Save(ctx, job, previous)
	}
	if job.State != "queued" || job.Plan == nil || s.Now().Before(job.AvailableAt) {
		return nil
	}
	if s.Authorize == nil || !s.Authorize(ctx, owner) {
		previous := job.Revision
		job.Revision++
		job.State = "cancelled"
		job.Problem = "voice_access_required"
		job.Plan = nil
		job.Policy = nil
		job.Progress = OrganizationState{}
		return s.Store.Save(ctx, job, previous)
	}
	work, err := job.Progress.Work(*job.Plan)
	if err != nil {
		return err
	}
	if s.Admit == nil {
		return ErrInvalid
	}
	frozen := ctx
	if job.Policy != nil {
		frozen = promptconfig.WithRevision(frozen, *job.Policy)
	}
	previous := job.Revision
	job.Revision++
	job.State = "running"
	job.LeaseUntil = s.Now().Add(5 * time.Minute)
	if err := s.Store.Save(ctx, job, previous); err != nil {
		return err
	}
	claimed := job.Revision
	settle, err := s.Admit(frozen, job, work.Snapshot)
	if err != nil {
		previous := job.Revision
		job.Revision++
		job.State = "queued"
		job.LeaseUntil = time.Time{}
		job.Problem = "voice_organization_unavailable"
		job.AvailableAt = s.Now().Add(5 * time.Second)
		if errors.Is(err, quota.ErrExceeded) {
			job.State = "failed"
			job.Problem = "voice_quota_exhausted"
		}
		if errors.Is(err, quota.ErrInvalidIdentity) {
			job.State = "failed"
		}
		return s.Store.Save(ctx, job, previous)
	}
	if settle == nil {
		return ErrInvalid
	}
	call, stop := context.WithTimeout(costcontrol.WithAccess(frozen, owner, true), 4*time.Minute)
	result, callErr := s.Model.Rewrite(call, work.Snapshot, 0)
	stop()
	settle(result, callErr)
	if s.Usage != nil {
		s.Usage(ctx, owner, uuid.NewSHA1(uuid.NameSpaceOID, []byte(id+"/"+strconv.FormatUint(claimed, 10))).String(), result)
	}
	current, err := s.Store.Get(ctx, owner, id)
	if err != nil {
		return err
	}
	// A cancellation/deletion is final even when the provider finishes later.
	if current.Revision != claimed || current.State != "running" {
		return nil
	}
	if s.Authorize == nil || !s.Authorize(ctx, owner) {
		_, err := s.Cancel(ctx, job.Principal, id, claimed)
		return err
	}
	current.Revision++
	current.LeaseUntil = time.Time{}
	current.AvailableAt = s.Now()
	if callErr != nil {
		current.State, current.Problem = "failed", "voice_rewrite_unavailable"
		if errors.Is(callErr, costcontrol.ErrConcurrencyExceeded) {
			current.State = "queued"
			current.AvailableAt = s.Now().Add(3 * time.Second)
		}
		if strings.HasPrefix(result.Diagnostics.Stage, "validate_polish") {
			current.Problem = "voice_rewrite_invalid_result"
		}
		if result.Diagnostics.Stage == "http_transport" || result.Diagnostics.Stage == "read_provider_response" || ctx.Err() != nil {
			current.State = "uncertain"
			current.Problem = "voice_rewrite_interrupted"
		}
	} else if err := current.Progress.Advance(*current.Plan, work, result.Polish); err != nil {
		current.State, current.Problem = "failed", "voice_rewrite_invalid_result"
	} else if current.Progress.Section == len(current.Plan.Sections) {
		current.State, current.Problem = "succeeded", ""
		current.Plan = nil
		current.Policy = nil
	} else {
		current.State, current.Problem = "queued", ""
	}
	// Deployment draining keeps this root context alive through settlement.
	return s.Store.Save(ctx, current, claimed)
}

func (s *OrganizationService) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); s.run(ctx) }()
	}
	workers.Wait()
}

func (s *OrganizationService) run(ctx context.Context) {
	for lifecycle.Wait(ctx) {
		jobs, err := s.Store.Pending(ctx, s.Now(), 16)
		if err != nil && ctx.Err() == nil {
			slog.Warn("Journal voice organization queue unavailable")
		}
		for _, job := range jobs {
			done, admitted := lifecycle.Begin(ctx)
			if !admitted {
				return
			}
			err := s.Process(ctx, job[0], job[1])
			done()
			if err != nil && !errors.Is(err, ErrOrganizationConflict) && !errors.Is(err, ErrOrganizationNotFound) && ctx.Err() == nil {
				slog.Warn("Journal voice organization settlement unavailable")
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
