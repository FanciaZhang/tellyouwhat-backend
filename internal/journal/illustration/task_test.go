package illustration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func confirmedTask(t *testing.T) Task {
	t.Helper()
	task, err := NewTask("journal", "owner", "image-model", Confirmation{
		RequestID: uuid.NewString(), VersionID: uuid.NewString(), Prompt: "private scene", ConsentRevision: "illustration-v1",
	}, nil, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestConfirmationBindsMaterialsAndConsent(t *testing.T) {
	image := testPNG(t)
	digest := sha256.Sum256(image)
	c := Confirmation{RequestID: uuid.NewString(), VersionID: uuid.NewString(), Prompt: "private scene", ConsentRevision: "v1", ReferenceSHA256: hex.EncodeToString(digest[:])}
	a, err := NewTask("journal", "owner", "model", c, image, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewTask("journal", "owner", "new-model-config", c, image, time.Unix(2000, 0))
	if !a.SameRequest(b) {
		t.Fatal("retry must recover original frozen model")
	}
	c.Prompt = "changed scene"
	b, _ = NewTask("journal", "owner", "model", c, image, time.Unix(1000, 0))
	if a.SameRequest(b) {
		t.Fatal("changed prompt reused identity")
	}
	b = a
	b.OwnerID = "other"
	if a.SameRequest(b) {
		t.Fatal("cross-owner match")
	}
	b = a
	b.AppID = "health"
	if a.SameRequest(b) {
		t.Fatal("cross-app match")
	}
	if c.Validate(nil) == nil || c.Validate([]byte("changed image")) == nil {
		t.Fatal("unconfirmed materials accepted")
	}
	c.ConsentRevision = ""
	if c.Validate(image) == nil {
		t.Fatal("missing consent accepted")
	}
	encoded, _ := json.Marshal(a)
	if strings.Contains(string(encoded), "private scene") {
		t.Fatal("task metadata contains prompt")
	}
}

func TestTaskCancellationAndLateCompletion(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		task := confirmedTask(t)
		if err := task.Start(1, task.CreatedAt); err != nil {
			t.Fatal(err)
		}
		if err := task.Start(2, task.CreatedAt); !errors.Is(err, ErrTaskConflict) {
			t.Fatal("duplicate dispatch")
		}
		if cancelFirst {
			if err := task.Cancel(task.Revision); err != nil {
				t.Fatal(err)
			}
		}
		result, _ := inspectImage(testPNG(t))
		deliver, err := task.RecordOutcome(task.Revision, &result, nil, task.CreatedAt)
		if err != nil || deliver == cancelFirst || task.Charge != ChargeOneImage {
			t.Fatal("incorrect cancellation settlement")
		}
		if !cancelFirst {
			if err := task.Cancel(task.Revision); err != nil {
				t.Fatal(err)
			}
		}
		if task.State != TaskCancelled || task.ResultDigest != "" {
			t.Fatal("cancelled content exposed")
		}
		if _, err := task.RecordOutcome(task.Revision, &result, nil, task.CreatedAt); !errors.Is(err, ErrTaskConflict) {
			t.Fatal("double settlement")
		}
	}
	task := confirmedTask(t)
	if err := task.Cancel(1); err != nil {
		t.Fatal(err)
	}
	if task.Charge != ChargeRelease || task.Start(task.Revision, task.CreatedAt) == nil {
		t.Fatal("cancel-before-dispatch failed")
	}
}

func TestTaskUnknownOutcomeNeverRestarts(t *testing.T) {
	for _, failure := range []error{ErrOutcomeUnknown, ErrResult, Rejected{Status: 429}} {
		task := confirmedTask(t)
		_ = task.Start(1, task.CreatedAt)
		if deliver, err := task.RecordOutcome(2, nil, failure, task.CreatedAt); err != nil || deliver {
			t.Fatal("failed result delivered")
		}
		if task.Charge != ChargeUncertain {
			t.Fatal("invented billing certainty")
		}
		if task.Start(task.Revision, task.CreatedAt) == nil {
			t.Fatal("uncertain task resubmitted")
		}
		// A later authoritative result reconciles the existing task, rather
		// than making another provider call or inventing a second request.
		result, _ := inspectImage(testPNG(t))
		if deliver, err := task.RecordOutcome(task.Revision, &result, nil, task.CreatedAt); err != nil || !deliver || task.Charge != ChargeOneImage {
			t.Fatal("reconciled result was lost")
		}
	}
}

func TestTaskExpirationAndStaleRevisionAreAtomic(t *testing.T) {
	task := confirmedTask(t)
	if task.Start(1, task.ExpiresAt) == nil {
		t.Fatal("expired dispatch")
	}
	_ = task.Start(1, task.CreatedAt)
	before := task
	if task.Cancel(1) == nil || task != before {
		t.Fatal("stale cancellation mutated task")
	}
	bad := Result{Image: []byte("bad")}
	if _, err := task.RecordOutcome(2, &bad, nil, task.CreatedAt); err == nil || task != before {
		t.Fatal("invalid result mutated task")
	}
	result, _ := inspectImage(testPNG(t))
	deliver, err := task.RecordOutcome(2, &result, nil, task.ExpiresAt)
	if err != nil || deliver || task.State != TaskCancelled || task.Charge != ChargeOneImage {
		t.Fatal("expired result exposed or charge lost")
	}
}
