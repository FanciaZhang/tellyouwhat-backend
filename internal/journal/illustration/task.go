package illustration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrTaskConflict = errors.New("illustration task changed")

// Confirmation binds explicit material consent to the exact frozen brief and
// reference. Owner/AppID come from authentication, never from request JSON.
type Confirmation struct {
	RequestID       string `json:"requestID"`
	VersionID       string `json:"versionID"`
	Prompt          string `json:"prompt"`
	ReferenceSHA256 string `json:"referenceSHA256,omitempty"`
	ConsentRevision string `json:"consentRevision"`
}

func (c Confirmation) Validate(reference []byte) error {
	r, er := uuid.Parse(c.RequestID)
	v, ev := uuid.Parse(c.VersionID)
	if er != nil || ev != nil || r == uuid.Nil || v == uuid.Nil || c.RequestID != r.String() || c.VersionID != v.String() ||
		!utf8.ValidString(c.Prompt) || strings.TrimSpace(c.Prompt) == "" || len([]rune(c.Prompt)) > 20_000 ||
		strings.TrimSpace(c.ConsentRevision) == "" || len(c.ConsentRevision) > 128 {
		return ErrInput
	}
	if len(reference) == 0 {
		if c.ReferenceSHA256 != "" {
			return ErrInput
		}
	} else {
		if _, err := inspectImage(reference); err != nil {
			return ErrInput
		}
		digest := sha256.Sum256(reference)
		if c.ReferenceSHA256 != hex.EncodeToString(digest[:]) {
			return ErrInput
		}
	}
	return nil
}

type TaskState string

const (
	TaskQueued    TaskState = "queued"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskRejected  TaskState = "rejected"
	TaskUncertain TaskState = "uncertain"
	TaskCancelled TaskState = "cancelled"
)

// ChargeState is separate from visible cancellation. Cancelling a dispatched
// request is not evidence that the upstream provider did not generate an image.
type ChargeState string

const (
	ChargeReserved  ChargeState = "reserved"
	ChargeUncertain ChargeState = "uncertain"
	ChargeRelease   ChargeState = "release"
	ChargeOneImage  ChargeState = "one_image"
)

// Task contains no raw prompt/image. Its payload and result must live in an
// encrypted short-lived store. Persist Revision with compare-and-swap before
// calling the provider; an expired worker is never automatically dispatched again.
type Task struct {
	ID, AppID, OwnerID, VersionID, RequestDigest, Model string
	BillingAccount, BudgetMonth                         string
	ConsentRevision                                     string
	State                                               TaskState
	Charge                                              ChargeState
	Revision                                            uint64
	CreatedAt, ExpiresAt                                time.Time
	ResultDigest                                        string
}

func NewTask(appID, ownerID, model string, c Confirmation, reference []byte, now time.Time) (Task, error) {
	if err := c.Validate(reference); err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(ownerID) == "" || strings.TrimSpace(model) == "" || now.IsZero() {
		return Task{}, ErrInput
	}
	encoded, _ := json.Marshal(c)
	hash := sha256.Sum256(encoded)
	return Task{ID: c.RequestID, AppID: appID, OwnerID: ownerID, VersionID: c.VersionID,
		RequestDigest: hex.EncodeToString(hash[:]), Model: model, ConsentRevision: c.ConsentRevision, State: TaskQueued,
		Charge: ChargeReserved, Revision: 1, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}, nil
}

func (t Task) SameRequest(other Task) bool {
	return t.ID == other.ID && t.AppID == other.AppID && t.OwnerID == other.OwnerID &&
		t.VersionID == other.VersionID && t.RequestDigest == other.RequestDigest && t.ConsentRevision == other.ConsentRevision
}

// AbortBeforeDispatch is only for the worker that owns the claim and has not
// invoked the provider. It must never be used after an ambiguous HTTP outcome.
func (t *Task) AbortBeforeDispatch() error {
	if (t.State != TaskRunning && t.State != TaskCancelled) || t.Charge != ChargeUncertain {
		return ErrTaskConflict
	}
	t.State, t.Charge, t.ResultDigest = TaskCancelled, ChargeRelease, ""
	t.Revision++
	return nil
}

func (t *Task) Start(expected uint64, now time.Time) error {
	if t.Revision != expected || t.State != TaskQueued || now.Before(t.CreatedAt) || !now.Before(t.ExpiresAt) {
		return ErrTaskConflict
	}
	t.State, t.Charge = TaskRunning, ChargeUncertain
	t.Revision++
	return nil
}

func (t *Task) Cancel(expected uint64) error {
	if t.State == TaskCancelled {
		return nil
	}
	if t.Revision != expected {
		return ErrTaskConflict
	}
	if t.State == TaskQueued {
		t.Charge = ChargeRelease
	}
	// Cancelling even a completed result revokes its delivery, not its charge.
	t.State, t.ResultDigest = TaskCancelled, ""
	t.Revision++
	return nil
}

// RecordOutcome returns whether the result may be delivered. Callers must save
// it atomically with image quota settlement and encrypted result publication.
// A cancellation racing with completion records usage but never restores content.
func (t *Task) RecordOutcome(expected uint64, result *Result, failure error, now time.Time) (bool, error) {
	if t.Revision != expected || now.Before(t.CreatedAt) || (result == nil) == (failure == nil) {
		return false, ErrTaskConflict
	}
	if t.State != TaskRunning && t.State != TaskUncertain && t.State != TaskRejected && t.State != TaskCancelled {
		return false, ErrTaskConflict
	}
	if t.Charge != ChargeUncertain {
		return false, ErrTaskConflict
	}
	cancelled := t.State == TaskCancelled || !now.Before(t.ExpiresAt)
	if result != nil {
		inspected, err := inspectImage(result.Image)
		if err != nil || inspected.MIME != result.MIME || inspected.Width != result.Width || inspected.Height != result.Height {
			return false, ErrResult
		}
		digest := sha256.Sum256(result.Image)
		t.Charge = ChargeOneImage
		if !cancelled {
			t.State, t.ResultDigest = TaskSucceeded, hex.EncodeToString(digest[:])
		}
	} else {
		// HTTP rejection is a user-visible failure, not authoritative billing
		// evidence. Settlement remains pending until provider reconciliation.
		var rejected Rejected
		if errors.As(failure, &rejected) && (rejected.Status == 400 || rejected.Status == 401 || rejected.Status == 403 || rejected.Status == 404 || rejected.Status == 413 || rejected.Status == 422 || rejected.Status == 429) {
			if !cancelled {
				t.State = TaskRejected
			}
		} else if !cancelled {
			t.State = TaskUncertain
		}
	}
	if cancelled {
		t.State, t.ResultDigest = TaskCancelled, ""
	}
	t.Revision++
	return t.State == TaskSucceeded, nil
}
