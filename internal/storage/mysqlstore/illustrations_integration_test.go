package mysqlstore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func TestIllustrationDurableClaimAndCancellation(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN required for real MySQL concurrency verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var databaseName string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(databaseName, "_test") {
		t.Fatal("requires isolated _test database")
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	app := "illustration-" + uuid.NewString()
	repository := mysqlstore.NewIllustrationRepository(db, cipher, app)
	c := illustration.Confirmation{RequestID: uuid.NewString(), VersionID: uuid.NewString(), Prompt: "private test scene", ConsentRevision: "v1"}
	firstID := c.RequestID
	defer func() {
		_, _ = db.Exec(`DELETE FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, firstID)
	}()
	now := time.Now().UTC()
	task, err := repository.CreateOrGet(ctx, "owner", "model", c, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh repository instance must recover the same frozen model.
	reopened := mysqlstore.NewIllustrationRepository(db, cipher, app)
	again, err := reopened.CreateOrGet(ctx, "owner", "new-model", c, nil, now)
	if err != nil || again.Model != task.Model {
		t.Fatal("request was not persisted")
	}
	if _, err := reopened.CreateOrGet(ctx, "other", "model", c, nil, now); err == nil {
		t.Fatal("cross-owner replay")
	}
	changed := c
	changed.Prompt = "different"
	if _, err := reopened.CreateOrGet(ctx, "owner", "model", changed, nil, now); err == nil {
		t.Fatal("changed request accepted")
	}
	var ciphertext []byte
	if err := db.QueryRowContext(ctx, `SELECT request_ciphertext FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, c.RequestID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(c.Prompt)) {
		t.Fatal("plaintext stored")
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, input, err := reopened.Claim(ctx, "owner", task.ID, 1, now)
			if err == nil {
				if input.Prompt != c.Prompt {
					t.Error("wrong payload")
				}
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("expected one durable claim, got %d", claimed.Load())
	}
	if _, err := repository.Cancel(ctx, "owner", task.ID, 1); err == nil {
		t.Fatal("stale cancellation")
	}
	cancelled, err := repository.Cancel(ctx, "owner", task.ID, 2)
	if err != nil || cancelled.State != illustration.TaskCancelled || cancelled.Charge != illustration.ChargeUncertain {
		t.Fatal("cancelled task incorrect")
	}
	if _, _, err := reopened.Claim(ctx, "owner", task.ID, cancelled.Revision, now); err == nil {
		t.Fatal("cancelled task dispatched")
	}
	var purged bool
	if err := db.QueryRowContext(ctx, `SELECT request_ciphertext IS NULL AND request_nonce IS NULL FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, c.RequestID).Scan(&purged); err != nil || !purged {
		t.Fatal("private payload not purged")
	}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 16, 12))); err != nil {
		t.Fatal(err)
	}
	result := illustration.Result{Image: imageBytes.Bytes(), MIME: "image/png", Width: 16, Height: 12}
	claimedTask := task
	if err := claimedTask.Start(1, now); err != nil {
		t.Fatal(err)
	}
	late, err := repository.StoreOutcome(ctx, claimedTask, &result, nil, now)
	if err != nil || late.State != illustration.TaskCancelled || late.Charge != illustration.ChargeOneImage {
		t.Fatal("late result accounting")
	}
	if _, err := repository.Result(ctx, "owner", task.ID, now); err == nil {
		t.Fatal("cancelled result delivered")
	}

	c.RequestID = uuid.NewString()
	c.VersionID = uuid.NewString()
	successID := c.RequestID
	defer func() {
		_, _ = db.Exec(`DELETE FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, successID)
	}()
	_, err = repository.CreateOrGet(ctx, "owner", "model", c, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	claimedTask, _, err = repository.Claim(ctx, "owner", successID, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := repository.StoreOutcome(ctx, claimedTask, &result, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := reopened.StoreOutcome(ctx, claimedTask, &result, nil, now)
	if err != nil || duplicate != completed {
		t.Fatal("completion replay mutated task")
	}
	got, err := reopened.Result(ctx, "owner", successID, now)
	if err != nil || !bytes.Equal(got.Image, result.Image) {
		t.Fatal("encrypted result roundtrip")
	}
	if _, err := reopened.Result(ctx, "other", successID, now); err == nil {
		t.Fatal("cross-owner result exposed")
	}
	if _, err := reopened.Result(ctx, "owner", successID, completed.ExpiresAt); err == nil {
		t.Fatal("expired result exposed")
	}
	if err := repository.PurgeExpiredPayloads(ctx, completed.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Result(ctx, "owner", successID, now); err == nil {
		t.Fatal("purged result recovered by backdated clock")
	}

	// Registered App Attest keys use up to 512 ASCII bytes in the shared
	// schema. Preserve that owner identity without truncation in this table.
	c.RequestID = uuid.NewString()
	c.VersionID = uuid.NewString()
	longOwnerID := c.RequestID
	defer func() {
		_, _ = db.Exec(`DELETE FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, longOwnerID)
	}()
	longOwner := strings.Repeat("k", 512)
	if _, err := repository.CreateOrGet(ctx, longOwner, "model", c, nil, now); err != nil {
		t.Fatal(err)
	}
	// A malformed nonce must fail closed, not panic inside AES-GCM.
	if _, err := db.ExecContext(ctx, `UPDATE journal_illustration_tasks SET request_nonce=? WHERE app_id=? AND id=?`, []byte{1}, app, longOwnerID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Claim(ctx, longOwner, longOwnerID, 1, now); err == nil {
		t.Fatal("corrupt encrypted request accepted")
	}
	preserved, err := repository.CreateOrGet(ctx, longOwner, "model", c, nil, now)
	if err != nil || preserved.State != illustration.TaskQueued || preserved.Revision != 1 {
		t.Fatal("failed decryption committed dispatch")
	}

	c.RequestID = uuid.NewString()
	c.VersionID = uuid.NewString()
	workerID := c.RequestID
	defer func() {
		_, _ = db.Exec(`DELETE FROM journal_illustration_tasks WHERE app_id=? AND id=?`, app, workerID)
	}()
	if _, err := repository.CreateOrGet(ctx, "owner", "model", c, nil, now); err != nil {
		t.Fatal(err)
	}
	generated := &illustrationTestGenerator{result: result}
	checks := 0
	worker := illustration.Worker{Store: repository, Generator: generated, Now: func() time.Time { return now }, Guard: illustrationTestGuard(func(_ context.Context, task illustration.Task) error {
		checks++
		if task.ConsentRevision != "v1" {
			return illustration.ErrInput
		}
		return nil
	})}
	finished, err := worker.Process(ctx, "owner", workerID)
	if err != nil || finished.State != illustration.TaskSucceeded || checks != 2 {
		t.Fatalf("worker pipeline: %v", err)
	}
	_, _ = worker.Process(ctx, "owner", workerID)
	if generated.calls != 1 {
		t.Fatal("worker redelivered upstream request")
	}
	if _, err := repository.Result(ctx, "owner", workerID, now); err != nil {
		t.Fatal(err)
	}
}

type illustrationTestGuard func(context.Context, illustration.Task) error

func (g illustrationTestGuard) Authorize(ctx context.Context, t illustration.Task) error {
	return g(ctx, t)
}

type illustrationTestGenerator struct {
	result illustration.Result
	calls  int
}

func (g *illustrationTestGenerator) Model() string { return "model" }
func (g *illustrationTestGenerator) Generate(context.Context, illustration.Input) (illustration.Result, error) {
	g.calls++
	return g.result, nil
}
