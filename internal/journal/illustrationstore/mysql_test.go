package illustrationstore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journal/illustrationstore"
	"github.com/tellyouwhat/backend/internal/lifecycle"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func imageFixture(t *testing.T) (illustrationstore.Store, illustration.Job, time.Time) {
	t.Helper()
	db := testutil.MySQL(t)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"owner", "other-owner"} {
		if err := mysqlstore.NewKeyRepository(db, "journal").Register(context.Background(), attestation.RegisteredKey{KeyID: owner, DeviceID: uuid.NewString(), PublicKey: []byte("synthetic"), Environment: "production", Receipt: []byte("synthetic")}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task, err := illustration.NewTask("journal", "owner", "image-model", illustration.Confirmation{RequestID: uuid.NewString(), VersionID: uuid.NewString(), Brief: illustration.Brief{Title: "private scene", Details: []illustration.Detail{{Kind: "other", Text: "private scene"}}}, ConsentRevision: "v1"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return illustrationstore.Store{Database: db, Cipher: cipher}, illustration.Job{Task: task, Input: &illustration.Input{Prompt: "private scene"}}, now
}

func TestMySQLIllustrationEncryptionIsolationAndAtomicClaim(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "other-owner", job.Task.ID); !errors.Is(err, illustration.ErrNotFound) {
		t.Fatal("cross-owner read", err)
	}
	var encrypted []byte
	if err := store.Database.QueryRow(`SELECT ciphertext FROM journal_illustration_jobs WHERE request_id=?`, job.Task.ID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte("private scene")) {
		t.Fatal("plaintext persisted", err)
	}
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal("idempotent replay", err)
	}
	changed := job
	changed.Task.RequestDigest = strings.Repeat("f", 64)
	if _, err := store.Create(ctx, changed); !errors.Is(err, illustration.ErrTaskConflict) {
		t.Fatal("changed request accepted", err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed := job
			claimed.Task.Start(1, now)
			claimed.LeaseUntil = now.Add(5 * time.Minute)
			err := store.Save(ctx, claimed, 1)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, illustration.ErrTaskConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("duplicate claims", winners.Load())
	}
	pending, err := store.Pending(ctx, now, 16)
	if err != nil || len(pending) != 0 {
		t.Fatal("unexpired claim redispatched", err)
	}
	pending, err = store.Pending(ctx, now.Add(6*time.Minute), 16)
	if err != nil || len(pending) != 1 {
		t.Fatal("crash recovery did not find stale claim", err)
	}
	// Swapping encrypted blobs between owners must fail authentication.
	clone := job
	clone.Task.OwnerID = "other-owner"
	if _, err = store.Create(ctx, clone); err != nil {
		t.Fatal(err)
	}
	_, err = store.Database.Exec(`UPDATE journal_illustration_jobs SET ciphertext=? WHERE owner_key_id=? AND request_id=?`, encrypted, "other-owner", job.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, "other-owner", job.Task.ID); err == nil {
		t.Fatal("ciphertext substitution accepted")
	}
}

type gatedImageGenerator struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	image   []byte
}

func (g *gatedImageGenerator) Model() string { return "image-model" }
func (g *gatedImageGenerator) Generate(ctx context.Context, _ illustration.Input) (illustration.Result, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return illustration.Result{}, ctx.Err()
	}
	return illustration.Result{Image: g.image, MIME: "image/png", Width: 12, Height: 8}, nil
}

func TestMySQLIllustrationOverlappingWorkersCancelAndLateResult(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	generator := &gatedImageGenerator{entered: make(chan struct{}), release: make(chan struct{}), image: b.Bytes()}
	s := &illustration.Service{Store: store, Generator: generator, Now: func() time.Time { return now }, Authorize: func(context.Context, string) bool { return true }}
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() { defer workers.Done(); s.Run(ctx) }()
	}
	defer func() { cancel(); workers.Wait() }()
	select {
	case <-generator.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	claimed, err := store.Get(ctx, job.Task.OwnerID, job.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cancel(ctx, job.Task.OwnerID, job.Task.ID, claimed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	close(generator.release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.Get(ctx, job.Task.OwnerID, job.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Task.Charge == illustration.ChargeOneImage {
			if current.Task.State != illustration.TaskCancelled || current.Result != nil || current.Input != nil || generator.calls.Load() != 1 {
				t.Fatal("late result resurrected or duplicated")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("known provider outcome was not settled")
}

func TestMySQLIllustrationCrashAndExpiryNeverRedispatch(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.Task.Start(1, now)
	job.LeaseUntil = now.Add(time.Minute)
	if err := store.Save(ctx, job, 1); err != nil {
		t.Fatal(err)
	}
	generator := &gatedImageGenerator{entered: make(chan struct{}), release: make(chan struct{})}
	s := &illustration.Service{Store: store, Generator: generator, Now: func() time.Time { return now.Add(6 * time.Minute) }, Authorize: func(context.Context, string) bool { return true }}
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.Get(ctx, job.Task.OwnerID, job.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Task.State == illustration.TaskUncertain {
			if current.Input != nil || current.Task.Charge != illustration.ChargeUncertain || generator.calls.Load() != 0 {
				t.Fatal("ambiguous provider outcome retried or made free")
			}
			cancel()
			<-done
			expired := &illustration.Service{Store: store, Now: func() time.Time { return now.Add(25 * time.Hour) }}
			current, err = expired.Get(context.Background(), job.Task.OwnerID, job.Task.ID)
			if err != nil || current.Task.State != illustration.TaskCancelled || current.Result != nil || current.Input != nil {
				t.Fatal("expired content retained", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stale task not recovered")
}

func TestMySQLIllustrationPrivacyDeletionFencesLateWrites(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := mysqlstore.NewPrivacyRepository(store.Database, "journal").DeletePrincipal(ctx, attestation.Principal{AppID: "journal", KeyID: job.Task.OwnerID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, job.Task.OwnerID, job.Task.ID); !errors.Is(err, illustration.ErrNotFound) {
		t.Fatal("private image retained", err)
	}
	claimed := job
	claimed.Task.Start(1, now)
	if err := store.Save(ctx, claimed, 1); !errors.Is(err, illustration.ErrTaskConflict) {
		t.Fatal("late settlement revived deleted task", err)
	}
	if _, err := store.Create(ctx, job); err == nil {
		t.Fatal("deleted key recreated private image task")
	}
}

func TestMySQLIllustrationMaintenanceWorksWithGenerationDisabled(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	metrics, err := (platformops.Store{DB: store.Database}).Metrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.Queues) != 1 || metrics.Queues[0].Operation != "journal.illustration" || metrics.Queues[0].Count != 1 {
		t.Fatal("image queue absent from operations", metrics.Queues)
	}
	count, err := store.Expire(ctx, now.Add(25*time.Hour))
	if err != nil || count != 1 {
		t.Fatal("retention cleanup failed", count, err)
	}
	replay, err := store.Create(ctx, job)
	if err != nil || replay.Task.State != illustration.TaskCancelled || replay.Input != nil || replay.Result != nil {
		t.Fatal("expired request generated a new task", err)
	}
	count, err = store.Expire(ctx, now.Add(25*time.Hour))
	if err != nil || count != 0 {
		t.Fatal("retention cleanup not idempotent", count, err)
	}
}

func TestMySQLIllustrationStandbyAndReleaseDrain(t *testing.T) {
	store, job, now := imageFixture(t)
	control, err := lifecycle.New("blue")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(lifecycle.WithController(context.Background(), control))
	defer cancel()
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	generator := &gatedImageGenerator{entered: make(chan struct{}), release: make(chan struct{}), image: imageBytes.Bytes()}
	service := &illustration.Service{Store: store, Generator: generator, Now: func() time.Time { return now }, Authorize: func(context.Context, string) bool { return true }}
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	command := func(action string) int {
		r := httptest.NewRequest("POST", "/control/"+action, strings.NewReader(`{"bootID":"`+control.Status().BootID+`"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		control.Control().ServeHTTP(w, r)
		return w.Code
	}
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-generator.entered:
		t.Fatal("standby dispatched image")
	case <-time.After(30 * time.Millisecond):
	}
	if command("serve") != 200 || command("resume") != 200 {
		t.Fatal("activation failed")
	}
	select {
	case <-generator.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("active image queue did not run")
	}
	if command("pause") != 200 || control.Status().Background != 1 || command("seal") != 409 {
		t.Fatal("release forgot in-flight provider call")
	}
	close(generator.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if control.Status().Background == 0 {
			if command("seal") != 200 {
				t.Fatal("settled image task prevented seal")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("image settlement did not drain")
}

func TestMySQLIllustrationUnavailableEntitlementsDoNotStarveQueue(t *testing.T) {
	store, job, now := imageFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 16 {
		queued := job
		queued.Task.ID = uuid.NewString()
		if _, err := store.Create(ctx, queued); err != nil {
			t.Fatal(err)
		}
	}
	job.Task.OwnerID = "other-owner"
	job.Task.ExpiresAt = job.Task.ExpiresAt.Add(time.Second)
	if _, err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	generator := &gatedImageGenerator{entered: make(chan struct{}), release: make(chan struct{})}
	service := &illustration.Service{Store: store, Generator: generator, Now: func() time.Time { return now }, Authorize: func(_ context.Context, owner string) bool { return owner == "other-owner" }}
	done := make(chan struct{})
	go func() { defer close(done); service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-generator.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unavailable entitlements starved an authorized job")
	}
	pending, err := store.Pending(ctx, now.Add(31*time.Second), 100)
	if err != nil || len(pending) != 16 {
		t.Fatal("deferred jobs were lost instead of retried", len(pending), err)
	}
	for _, task := range pending {
		stored, err := store.Get(ctx, task.OwnerID, task.ID)
		if err != nil || stored.Task.State != illustration.TaskQueued || stored.Input == nil {
			t.Fatal("authorization lookup discarded user input", err)
		}
	}
}
