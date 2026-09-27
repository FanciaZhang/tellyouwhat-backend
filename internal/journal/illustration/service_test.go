package illustration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/privacy"
)

type clientFixture struct {
	task                    Task
	denied                  bool
	reads, creates, cancels int
	afterResult             func()
}

func (f *clientFixture) ImageAllowance(context.Context, string, time.Time) (string, int64, error) {
	if f.denied {
		return "", 0, ErrAccessDenied
	}
	return "account", 5, nil
}
func (f *clientFixture) AuthorizeRead(context.Context, Task) error {
	if f.denied {
		return ErrAccessDenied
	}
	return nil
}
func (f *clientFixture) CreateOrGet(_ context.Context, owner, model string, c Confirmation, b []byte, now time.Time) (Task, error) {
	f.creates++
	return NewTask("journal", owner, model, c, b, now)
}
func (f *clientFixture) GetTask(_ context.Context, owner, id string, _ time.Time) (Task, error) {
	if f.task.OwnerID != owner || f.task.ID != id {
		return Task{}, ErrAccessDenied
	}
	return f.task, nil
}
func (f *clientFixture) Cancel(_ context.Context, owner, id string, revision uint64) (Task, error) {
	if f.task.OwnerID != owner || f.task.ID != id {
		return Task{}, ErrAccessDenied
	}
	f.cancels++
	err := f.task.Cancel(revision)
	return f.task, err
}
func (f *clientFixture) Result(context.Context, string, string, time.Time) (Result, error) {
	f.reads++
	if f.afterResult != nil {
		f.afterResult()
	}
	return Result{Image: []byte("private")}, nil
}

func TestClientServiceAuthorizationAndCancellation(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	c := Confirmation{RequestID: uuid.NewString(), VersionID: uuid.NewString(), Prompt: "一棵树", ConsentRevision: privacy.JournalIllustrationDocumentVersion}
	f := &clientFixture{}
	s := Service{Store: f, Access: f, Model: "configured-model", Now: func() time.Time { return now }}
	p := attestation.Principal{AppID: "journal", KeyID: "owner"}
	task, err := s.Create(ctx, p, c, nil)
	if err != nil || task.Model != s.Model || task.OwnerID != p.KeyID || f.creates != 1 {
		t.Fatalf("create: %v", err)
	}
	f.task = task
	health := p
	health.AppID = "health"
	if _, err = s.Create(ctx, health, c, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("Health creation permitted")
	}
	f.denied = true
	if _, err = s.Create(ctx, p, c, nil); !errors.Is(err, ErrAccessDenied) || f.creates != 1 {
		t.Fatal("revoked creation reached repository")
	}
	if _, err = s.Status(ctx, p, task.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("revoked status released")
	}
	if _, err = s.Result(ctx, p, task.ID); !errors.Is(err, ErrAccessDenied) || f.reads != 0 {
		t.Fatal("revoked result read")
	}
	if _, err = s.Cancel(ctx, p, task.ID, task.Revision); err != nil || f.task.State != TaskCancelled {
		t.Fatal("revoked owner cannot cancel")
	}
	other := p
	other.KeyID = "other"
	if _, err = s.Cancel(ctx, other, task.ID, task.Revision); err == nil {
		t.Fatal("cross owner cancellation")
	}
	if _, err = s.Cancel(ctx, health, task.ID, task.Revision); err == nil {
		t.Fatal("Health cancellation")
	}
	if _, err = s.Cancel(ctx, p, "not-an-id", 1); !errors.Is(err, ErrInput) {
		t.Fatal("invalid id")
	}
}

func TestClientResultRechecksAfterDecryption(t *testing.T) {
	for _, change := range []string{"none", "revoke", "cancel", "revision"} {
		t.Run(change, func(t *testing.T) {
			f := &clientFixture{task: Task{ID: uuid.NewString(), AppID: "journal", OwnerID: "owner", State: TaskSucceeded, Revision: 3, ResultDigest: "digest"}}
			f.afterResult = func() {
				switch change {
				case "revoke":
					f.denied = true
				case "cancel":
					_ = f.task.Cancel(3)
				case "revision":
					f.task.Revision++
				}
			}
			s := Service{Store: f, Access: f}
			result, err := s.Result(context.Background(), attestation.Principal{AppID: "journal", KeyID: "owner"}, f.task.ID)
			if change == "none" {
				if err != nil || len(result.Image) == 0 {
					t.Fatal("valid result denied")
				}
			} else if err == nil || len(result.Image) != 0 {
				t.Fatal("private bytes released after change")
			}
		})
	}
}
