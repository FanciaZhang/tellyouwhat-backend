package illustration

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type ClientRepository interface {
	CreateOrGet(context.Context, string, string, Confirmation, []byte, time.Time) (Task, error)
	GetTask(context.Context, string, string, time.Time) (Task, error)
	Cancel(context.Context, string, string, uint64) (Task, error)
	Result(context.Context, string, string, time.Time) (Result, error)
}

type ClientAccess interface {
	ImageAllowance(context.Context, string, time.Time) (string, int64, error)
	AuthorizeRead(context.Context, Task) error
}

// Service accepts only an authenticated principal from the gateway. Creating a
// task persists it; it never launches an untracked goroutine or repeats dispatch.
type Service struct {
	Store  ClientRepository
	Access ClientAccess
	Model  string
	Now    func() time.Time
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Service) check(principal attestation.Principal, id string) error {
	if principal.AppID != "journal" || principal.KeyID == "" {
		return ErrAccessDenied
	}
	if s.Store == nil || s.Access == nil {
		return ErrWorkerUnavailable
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return ErrInput
	}
	return nil
}

func (s Service) Create(ctx context.Context, principal attestation.Principal, confirmation Confirmation, reference []byte) (Task, error) {
	if err := s.check(principal, confirmation.RequestID); err != nil {
		return Task{}, err
	}
	if confirmation.ConsentRevision != privacy.JournalIllustrationDocumentVersion {
		return Task{}, ErrAccessDenied
	}
	if err := confirmation.Validate(reference); err != nil {
		return Task{}, err
	}
	if s.Model == "" {
		return Task{}, ErrWorkerUnavailable
	}
	now := s.now()
	if _, _, err := s.Access.ImageAllowance(ctx, principal.KeyID, now); err != nil {
		return Task{}, err
	}
	return s.Store.CreateOrGet(ctx, principal.KeyID, s.Model, confirmation, reference, now)
}

func (s Service) Status(ctx context.Context, principal attestation.Principal, id string) (Task, error) {
	if err := s.check(principal, id); err != nil {
		return Task{}, err
	}
	task, err := s.Store.GetTask(ctx, principal.KeyID, id, s.now())
	if err != nil {
		return Task{}, err
	}
	if task.AppID != principal.AppID || task.OwnerID != principal.KeyID || task.ID != id {
		return Task{}, ErrAccessDenied
	}
	if err := s.Access.AuthorizeRead(ctx, task); err != nil {
		return Task{}, err
	}
	return task, nil
}

// Cancellation remains available after consent withdrawal/subscription expiry.
// Ownership and revision are enforced transactionally by the repository.
func (s Service) Cancel(ctx context.Context, principal attestation.Principal, id string, revision uint64) (Task, error) {
	if err := s.check(principal, id); err != nil {
		return Task{}, err
	}
	if revision == 0 {
		return Task{}, ErrInput
	}
	return s.Store.Cancel(ctx, principal.KeyID, id, revision)
}

func (s Service) Result(ctx context.Context, principal attestation.Principal, id string) (Result, error) {
	task, err := s.Status(ctx, principal, id)
	if err != nil {
		return Result{}, err
	}
	if task.State != TaskSucceeded {
		return Result{}, ErrTaskConflict
	}
	result, err := s.Store.Result(ctx, principal.KeyID, id, s.now())
	if err != nil {
		return Result{}, err
	}
	// Recheck after decryption, before releasing private bytes to HTTP transport.
	current, err := s.Status(ctx, principal, id)
	if err != nil {
		return Result{}, err
	}
	if current.Revision != task.Revision || current.State != TaskSucceeded || current.ResultDigest != task.ResultDigest {
		return Result{}, ErrTaskConflict
	}
	return result, nil
}
