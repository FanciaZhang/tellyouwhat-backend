package voicestore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/journal/voicestore"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

type rewriteFunc func(context.Context, voice.Snapshot) (voice.RewriteResult, error)

func (f rewriteFunc) Rewrite(ctx context.Context, s voice.Snapshot, _ int) (voice.RewriteResult, error) {
	return f(ctx, s)
}

func fixture(t *testing.T, count int) (voicestore.Store, *voice.OrganizationService, attestation.Principal, voice.OrganizationJob) {
	t.Helper()
	db := testutil.MySQL(t)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	p := attestation.Principal{AppID: "journal", KeyID: "owner", DeviceID: uuid.NewString()}
	for _, owner := range []string{"owner", "other"} {
		if err := mysqlstore.NewKeyRepository(db, "journal").Register(t.Context(), attestation.RegisteredKey{KeyID: owner, DeviceID: uuid.NewString(), PublicKey: []byte("synthetic"), Environment: "production", Receipt: []byte("synthetic")}); err != nil {
			t.Fatal(err)
		}
	}
	store := voicestore.Store{Database: db, Cipher: cipher}
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &voice.OrganizationService{Store: store, Now: func() time.Time { return now }, Authorize: func(context.Context, string) bool { return true },
		Admit: func(context.Context, voice.OrganizationJob, voice.Snapshot) (func(voice.RewriteResult, error), error) {
			return func(voice.RewriteResult, error) {}, nil
		}}
	service.Model = rewriteFunc(func(_ context.Context, s voice.Snapshot) (voice.RewriteResult, error) {
		text, ids := "", []string{}
		for _, t := range s.Polish.Targets {
			text += t.RetainedText + t.SourceText
			ids = append(ids, t.ID)
		}
		return voice.RewriteResult{Polish: &voice.PolishRevision{Targets: s.Polish.Targets, Paragraphs: []voice.PolishParagraph{{Text: text, Style: "body", TargetIDs: ids}}}}, nil
	})
	author := uuid.NewString()
	snapshot := voice.Snapshot{DictationMode: true, WritingStyle: "natural"}
	snapshot.Polish = &voice.PolishRequest{Narrator: &voice.Narrator{PersonID: author, Name: "我"}}
	snapshot.Narrator = snapshot.Polish.Narrator
	for range count {
		turn := voice.SourceUtterance{ID: uuid.NewString(), Text: "private source，预算280元，还没付款。", PersonID: author, Person: "我"}
		target := voice.PolishTarget{ID: uuid.NewString(), Text: turn.Text, SourceText: turn.Text, SourceIDs: []string{turn.ID}, Turns: []voice.SourceUtterance{turn}, Style: "body", CompleteSource: true}
		snapshot.Polish.Targets = append(snapshot.Polish.Targets, target)
	}
	plan := voice.OrganizationPlan{Version: voice.OrganizationVersion, Sections: []voice.Snapshot{snapshot}}
	data, _ := json.Marshal(plan)
	hash := sha256.Sum256(data)
	job, err := service.Create(t.Context(), p, uuid.NewString(), hex.EncodeToString(hash[:]), data, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return store, service, p, job
}

func TestMySQLVoiceOrganizationEncryptionOwnershipAdmissionAndExpiry(t *testing.T) {
	store, service, p, job := fixture(t, 1)
	ctx := t.Context()
	var encrypted []byte
	if err := store.Database.QueryRow(`SELECT ciphertext FROM journal_voice_organization_jobs WHERE request_id=?`, job.ID).Scan(&encrypted); err != nil || bytes.Contains(encrypted, []byte("private source")) {
		t.Fatal("private source persisted in plaintext", err)
	}
	if _, err := service.Get(ctx, "other", job.ID); !errors.Is(err, voice.ErrOrganizationNotFound) {
		t.Fatal("cross-owner read", err)
	}
	data, _ := json.Marshal(job.Plan)
	if repeated, err := service.Create(ctx, p, job.ID, job.Digest, data, "127.0.0.1"); err != nil || repeated.Revision != 1 {
		t.Fatal("idempotency", err)
	}
	var winners atomic.Int32
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claimed := job
			claimed.Revision++
			claimed.State = "running"
			claimed.LeaseUntil = service.Now().Add(time.Minute)
			if store.Save(ctx, claimed, 1) == nil {
				winners.Add(1)
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatal("duplicate worker claims", winners.Load())
	}
	restarted := voicestore.Store{Database: store.Database, Cipher: store.Cipher}
	restored, err := restarted.Get(ctx, p.KeyID, job.ID)
	if err != nil || restored.Plan == nil || restored.Revision != 2 {
		t.Fatal("restart recovery", err)
	}
	newer := job
	newer.ID = uuid.NewString()
	newer.CreatedAt = newer.CreatedAt.Add(time.Hour)
	newer.ExpiresAt = newer.ExpiresAt.Add(time.Hour)
	if _, err := store.Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if count, err := store.Expire(ctx, job.ExpiresAt); err != nil || count != 1 {
		t.Fatal("expiry", count, err)
	}
	expired, err := store.Get(ctx, p.KeyID, job.ID)
	if err != nil || expired.State != "expired" || expired.Plan != nil || len(expired.Progress.Results) != 0 {
		t.Fatal("expired payload retained", err)
	}
	if active, err := store.Get(ctx, p.KeyID, newer.ID); err != nil || active.Plan == nil {
		t.Fatal("active task was removed", err)
	}
	if err := store.ForgetOwner(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, p.KeyID, newer.ID); !errors.Is(err, voice.ErrOrganizationNotFound) {
		t.Fatal("owner deletion incomplete", err)
	}
}

func TestMySQLVoiceOrganizationRunsAfterClientDisconnectAndResumesSavedBatches(t *testing.T) {
	store, service, p, job := fixture(t, 25)
	createdContext, cancel := context.WithCancel(t.Context())
	data, _ := json.Marshal(job.Plan)
	job.ID = uuid.NewString()
	var err error
	job, err = service.Create(createdContext, p, job.ID, job.Digest, data, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	var calls atomic.Int32
	original := service.Model
	service.Model = rewriteFunc(func(ctx context.Context, s voice.Snapshot) (voice.RewriteResult, error) {
		calls.Add(1)
		return original.Rewrite(ctx, s, 0)
	})
	if err := service.Process(t.Context(), p.KeyID, job.ID); err != nil {
		t.Fatal(err)
	}
	partial, err := store.Get(t.Context(), p.KeyID, job.ID)
	if err != nil || partial.Progress.Next == 0 || partial.State != "queued" {
		t.Fatal("batch was not durably saved", err)
	}
	next := *service
	next.Store = voicestore.Store{Database: store.Database, Cipher: store.Cipher}
	for i := 0; i < 10; i++ {
		state, err := next.Get(t.Context(), p.KeyID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == "succeeded" {
			if len(state.Progress.Results) != 1 || len(state.Progress.Results[0].Targets) != 25 {
				t.Fatal("incomplete result")
			}
			text := ""
			for _, paragraph := range state.Progress.Results[0].Paragraphs {
				text += paragraph.Text
			}
			if strings.Count(text, "private source") != 25 {
				t.Fatal("speech repeated or lost")
			}
			if calls.Load() > 5 {
				t.Fatal("completed batches were paid for again")
			}
			return
		}
		if err := next.Process(t.Context(), p.KeyID, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("organization did not finish")
}

func TestMySQLVoiceOrganizationCancellationDeletionAndUncertainRetry(t *testing.T) {
	store, service, p, job := fixture(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	original := service.Model
	var calls atomic.Int32
	service.Model = rewriteFunc(func(ctx context.Context, s voice.Snapshot) (voice.RewriteResult, error) {
		calls.Add(1)
		close(started)
		<-release
		return original.Rewrite(ctx, s, 0)
	})
	done := make(chan error, 1)
	go func() { done <- service.Process(t.Context(), p.KeyID, job.ID) }()
	<-started
	current, err := service.Get(t.Context(), p.KeyID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cancel(t.Context(), p, job.ID, current.Revision); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err = service.Get(t.Context(), p.KeyID, job.ID)
	if err != nil || current.State != "cancelled" || current.Plan != nil || len(current.Progress.Results) > 0 {
		t.Fatal("late result resurrected content", err)
	}
	// A lost worker is never silently resubmitted to a paid provider.
	other := job
	other.ID = uuid.NewString()
	if _, err := store.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	other.State = "running"
	other.Revision++
	other.LeaseUntil = service.Now().Add(-time.Second)
	if err := store.Save(t.Context(), other, 1); err != nil {
		t.Fatal(err)
	}
	if err := service.Process(t.Context(), p.KeyID, other.ID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := service.Get(t.Context(), p.KeyID, other.ID)
	if err != nil || uncertain.State != "uncertain" || calls.Load() != 1 {
		t.Fatal("uncertain attempt was replayed", err)
	}
	if _, err := service.Retry(t.Context(), p.KeyID, other.ID, uncertain.Revision); err != nil {
		t.Fatal(err)
	}
	service.Model = original
	if err := service.Process(t.Context(), p.KeyID, other.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := service.Get(t.Context(), p.KeyID, other.ID); err != nil || ready.State != "succeeded" {
		t.Fatal("explicit recovery failed", err)
	}
	third := job
	third.ID = uuid.NewString()
	if _, err := store.Create(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	service.Admit = func(context.Context, voice.OrganizationJob, voice.Snapshot) (func(voice.RewriteResult, error), error) {
		return nil, quota.ErrExceeded
	}
	if err := service.Process(t.Context(), p.KeyID, third.ID); err != nil {
		t.Fatal(err)
	}
	if denied, err := service.Get(t.Context(), p.KeyID, third.ID); err != nil || denied.State != "failed" || denied.Problem != "voice_quota_exhausted" {
		t.Fatal("quota failure hidden as progress", err)
	}
	if err := store.ForgetOwner(t.Context(), p.KeyID); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLVoiceOrganizationSingleAdmissionAndDeletionDuringCall(t *testing.T) {
	store, service, p, job := fixture(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	original := service.Model
	var admits, calls atomic.Int32
	service.Admit = func(context.Context, voice.OrganizationJob, voice.Snapshot) (func(voice.RewriteResult, error), error) {
		admits.Add(1)
		return func(voice.RewriteResult, error) {}, nil
	}
	service.Model = rewriteFunc(func(ctx context.Context, s voice.Snapshot) (voice.RewriteResult, error) {
		calls.Add(1)
		close(started)
		<-release
		return original.Rewrite(ctx, s, 0)
	})
	done := make(chan error, 1)
	go func() { done <- service.Process(t.Context(), p.KeyID, job.ID) }()
	<-started
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := service.Process(t.Context(), p.KeyID, job.ID)
			if err != nil && !errors.Is(err, voice.ErrOrganizationConflict) {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if admits.Load() != 1 || calls.Load() != 1 {
		t.Fatal("duplicate admission or paid dispatch", admits.Load(), calls.Load())
	}
	if err := store.ForgetOwner(t.Context(), p.KeyID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil && !errors.Is(err, voice.ErrOrganizationNotFound) {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), p.KeyID, job.ID); !errors.Is(err, voice.ErrOrganizationNotFound) {
		t.Fatal("late provider result recreated erased owner", err)
	}
}

func TestMySQLVoiceCancellationBeforeCreateCannotDispatchLater(t *testing.T) {
	store, service, p, job := fixture(t, 1)
	data, _ := json.Marshal(job.Plan)
	id := uuid.NewString()
	cancelled, err := service.Cancel(t.Context(), p, id, 1)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatal("cancellation tombstone", err)
	}
	if _, err := service.Create(t.Context(), p, id, job.Digest, data, "127.0.0.1"); !errors.Is(err, voice.ErrOrganizationConflict) {
		t.Fatal("late create resurrected cancelled task", err)
	}
	if err := service.Process(t.Context(), p.KeyID, id); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(t.Context(), p.KeyID, id)
	if err != nil || current.Plan != nil || current.State != "cancelled" {
		t.Fatal("cancelled task regained payload", err)
	}
}
