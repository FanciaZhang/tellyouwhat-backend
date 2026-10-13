package albums

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type liveMultipartProviderFixture struct {
	*COSObjects
	initiations, completions, aborts, finds, verifications atomic.Int32
	initiate                                               func(context.Context, string) (string, error)
	complete                                               func(context.Context, string, string, int64) (string, error)
	abort                                                  func(context.Context) error
	find                                                   func(context.Context, string) (string, error)
	verify                                                 func(context.Context, string, Resource) (string, error)
}

func (p *liveMultipartProviderFixture) InitiateStagingMultipart(ctx context.Context, key string) (string, error) {
	p.initiations.Add(1)
	if p.initiate != nil {
		return p.initiate(ctx, key)
	}
	return "provider-" + key[strings.LastIndex(key, "/")+1:], nil
}
func (p *liveMultipartProviderFixture) CompleteStagingMultipart(ctx context.Context, key, id string, size int64) (string, error) {
	p.completions.Add(1)
	if p.complete != nil {
		return p.complete(ctx, key, id, size)
	}
	return "version-" + id, nil
}
func (p *liveMultipartProviderFixture) FindStagingMultipart(ctx context.Context, key string) (string, error) {
	p.finds.Add(1)
	if p.find != nil {
		return p.find(ctx, key)
	}
	return "provider-" + key[strings.LastIndex(key, "/")+1:], nil
}
func (p *liveMultipartProviderFixture) VerifyStagingResource(ctx context.Context, key string, r Resource) (string, error) {
	p.verifications.Add(1)
	if p.verify != nil {
		return p.verify(ctx, key, r)
	}
	return "version-provider-" + r.ID, nil
}
func (p *liveMultipartProviderFixture) ListStagingMultipartParts(_ context.Context, _ string, _ string, size int64) ([]MultipartUploadedPart, error) {
	return []MultipartUploadedPart{{1, size}}, nil
}
func (p *liveMultipartProviderFixture) abortPurgeUpload(ctx context.Context, _ string, _ purgeTarget, _ string) error {
	p.aborts.Add(1)
	if p.abort != nil {
		return p.abort(ctx)
	}
	return nil
}
func liveMultipartFixture(t *testing.T) (*pushFixture, *LivePairMultipartService, *UploadService, LivePairUpload, *liveMultipartProviderFixture) {
	t.Helper()
	f := newPushFixture(t)
	r := NewMySQLUploads(f.r.db)
	u, d := livePairJournalFixture(t, f.r.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	uploads := livePairTestService(t, r, &now, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider network"); return nil, nil })
	pair, err := uploads.CreateLivePair(context.Background(), u.OwnerID, u.ID, uuid.NewString(), d)
	if err != nil {
		t.Fatal(err)
	}
	p := &liveMultipartProviderFixture{COSObjects: uploads.objects.(*COSObjects)}
	service, err := newLivePairMultipartService(r, p, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return f, service, uploads, pair, p
}
func TestMySQLLiveMultipartPairAssemblesBeforeSubmission(t *testing.T) {
	_, s, uploads, u, p := liveMultipartFixture(t)
	ctx := context.Background()
	for _, part := range []string{"photo", "motion"} {
		a, err := s.Initialize(ctx, u.OwnerID, u.ID, part)
		if err != nil || a.ResourceID != part || a.PartCount != 1 {
			t.Fatal(a, err)
		}
		if _, err = s.Initialize(ctx, u.OwnerID, u.ID, part); err != nil {
			t.Fatal(err)
		}
		grant, err := s.Part(ctx, u.OwnerID, u.ID, part, 1)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(grant.URL)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := u.key(part, true)
		expected, _ := u.resource(part)
		if parsed.Path != "/"+key || parsed.Query().Get("uploadId") != "provider-"+part || grant.SizeBytes != expected.SizeBytes || !strings.Contains(parsed.Query().Get("q-url-param-list"), "uploadid") {
			t.Fatal("unbound part")
		}
		status, err := s.Status(ctx, u.OwnerID, u.ID, part)
		if err != nil || status.State != "uploading" || len(status.Parts) != 1 {
			t.Fatal(status, err)
		}
		if _, err = uploads.LivePairGrants(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("mixed direct PUT accepted", err)
		}
		if _, err = uploads.SubmitLivePair(ctx, u.OwnerID, u.ID); !errors.Is(err, ErrMultipartPending) {
			t.Fatal("unassembled pair submitted", err)
		}
		if _, err = s.Complete(ctx, u.OwnerID, u.ID, part); err != nil {
			t.Fatal(err)
		}
		reopened, _ := newLivePairMultipartService(NewMySQLUploads(s.repository.db), p, s.now)
		if _, err = reopened.Complete(ctx, u.OwnerID, u.ID, part); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Part(ctx, u.OwnerID, u.ID, part, 1); !errors.Is(err, ErrConflict) {
			t.Fatal("part authorized after Complete", err)
		}
	}
	if p.initiations.Load() != 2 || p.completions.Load() != 2 {
		t.Fatal("duplicate provider mutation")
	}
	if directUploadIssued(t, s.repository, "pair", u.ID) {
		t.Fatal("multipart marked direct")
	}
	if pair, err := uploads.SubmitLivePair(ctx, u.OwnerID, u.ID); err != nil || pair.State != "queued" {
		t.Fatal(pair.State, err)
	}
}
func TestMySQLLiveMultipartUnknownRecoveryDoesNotRepeatWrites(t *testing.T) {
	for _, phase := range []string{"init", "complete"} {
		t.Run(phase, func(t *testing.T) {
			_, s, _, u, p := liveMultipartFixture(t)
			ctx := context.Background()
			if phase == "init" {
				p.initiate = func(context.Context, string) (string, error) { return "", io.ErrUnexpectedEOF }
			} else {
				p.complete = func(context.Context, string, string, int64) (string, error) { return "", ErrCOSMultipartUnknown }
			}
			_, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo")
			if phase == "init" {
				if !errors.Is(err, ErrMultipartPending) {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.Complete(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrMultipartPending) {
					t.Fatal(err)
				}
			}
			for range 2 {
				if phase == "init" {
					_, err = s.Initialize(ctx, u.OwnerID, u.ID, "photo")
				} else {
					_, err = s.Complete(ctx, u.OwnerID, u.ID, "photo")
				}
				if !errors.Is(err, ErrMultipartPending) {
					t.Fatal(err)
				}
			}
			if err = s.Reconcile(ctx, u.OwnerID, u.ID, "photo"); err != nil {
				t.Fatal(err)
			}
			_, m, err := s.repository.liveMultipart(ctx, u.OwnerID, u.ID, "photo")
			if err != nil {
				t.Fatal(err)
			}
			want := "ready"
			if phase == "complete" {
				want = "assembled"
			}
			if m.State != want {
				t.Fatal(m.State)
			}
			if p.initiations.Load() != 1 || p.completions.Load() > 1 {
				t.Fatal("recovery repeated provider write")
			}
		})
	}
}
func TestMySQLLiveMultipartPreflightRetryAndStaleReceipt(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	ctx := context.Background()
	if _, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo"); err != nil {
		t.Fatal(err)
	}
	p.complete = func(context.Context, string, string, int64) (string, error) { return "", ErrIncompleteBackup }
	if _, err := s.Complete(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatal(err)
	}
	_, m, _, err := s.repository.claimLiveMultipartOperation(ctx, u.OwnerID, u.ID, "photo", false, s.now())
	if err != nil {
		t.Fatal(err)
	}
	wrong := m
	wrong.OperationID = uuid.NewString()
	if _, err = s.repository.resolveLiveMultipartOperation(ctx, u.OwnerID, wrong, "assembled", "version", s.now()); !errors.Is(err, ErrConflict) {
		t.Fatal("stale completion accepted", err)
	}
	if _, err = s.repository.resolveLiveMultipartOperation(ctx, u.OwnerID, m, "assembled", "version", s.now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.repository.resolveLiveMultipartOperation(ctx, u.OwnerID, m, "completion_unknown", "", s.now()); !errors.Is(err, ErrConflict) {
		t.Fatal("verified receipt lost", err)
	}
}
func TestMySQLLiveMultipartDeletionAbortsReadyButNotCompleting(t *testing.T) {
	for _, phase := range []string{"ready", "completing", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			f, s, _, u, p := liveMultipartFixture(t)
			ctx := context.Background()
			if _, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo"); err != nil {
				t.Fatal(err)
			}
			if err := s.AbortForDeletion(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrConflict) {
				t.Fatal("active account aborted", err)
			}
			if phase != "ready" {
				_, m, _, err := s.repository.claimLiveMultipartOperation(ctx, u.OwnerID, u.ID, "photo", false, s.now())
				if err != nil {
					t.Fatal(err)
				}
				if phase == "unknown" {
					if _, err = s.repository.resolveLiveMultipartOperation(ctx, u.OwnerID, m, "completion_unknown", "", s.now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			deleteProtocolFixtureOwner(t, f, u.OwnerID)
			if _, err := s.Part(ctx, u.OwnerID, u.ID, "photo", 1); err == nil {
				t.Fatal("deleting account got grant")
			}
			if _, err := s.Initialize(ctx, u.OwnerID, u.ID, "motion"); err == nil {
				t.Fatal("deleting account started upload")
			}
			if phase == "ready" {
				p.abort = func(context.Context) error { return io.ErrUnexpectedEOF }
				if err := s.AbortForDeletion(ctx, u.OwnerID, u.ID, "photo"); err == nil {
					t.Fatal("unknown abort became successful")
				}
				p.abort = nil
				if err := s.AbortForDeletion(ctx, u.OwnerID, u.ID, "photo"); err != nil {
					t.Fatal(err)
				}
				if err := s.AbortForDeletion(ctx, u.OwnerID, u.ID, "photo"); err != nil {
					t.Fatal(err)
				}
				if p.aborts.Load() != 2 {
					t.Fatal("abort receipt ignored")
				}
			} else {
				if err := s.AbortForDeletion(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrMultipartPending) {
					t.Fatal("Complete guessed stopped", err)
				}
				if p.aborts.Load() != 0 {
					t.Fatal("aborted in-flight Complete")
				}
				if err := s.Reconcile(ctx, u.OwnerID, u.ID, "photo"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestMySQLLiveMultipartCancelledClientRetainsLateReceipts(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	p.initiate = func(context.Context, string) (string, error) { cancel(); return "provider-photo", nil }
	if _, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, m, err := s.repository.liveMultipart(context.Background(), u.OwnerID, u.ID, "photo")
	if err != nil || m.State != "ready" {
		t.Fatal(m, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	p.complete = func(context.Context, string, string, int64) (string, error) { cancel(); return "version-photo", nil }
	if _, err = s.Complete(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, m, err = s.repository.liveMultipart(context.Background(), u.OwnerID, u.ID, "photo")
	if err != nil || m.State != "assembled" {
		t.Fatal(m, err)
	}
}
func TestMySQLLiveMultipartGrantRaceAndAccountIsolation(t *testing.T) {
	_, s, uploads, u, p := liveMultipartFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var direct, initialize error
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, direct = uploads.LivePairGrants(ctx, u.OwnerID, u.ID) }()
	go func() { defer wg.Done(); <-start; _, initialize = s.Initialize(ctx, u.OwnerID, u.ID, "photo") }()
	close(start)
	wg.Wait()
	if (direct == nil) == (initialize == nil) {
		t.Fatal("protocol race did not choose one", direct, initialize)
	}
	if direct == nil && p.initiations.Load() != 0 {
		t.Fatal("direct upload also initialized")
	}
	for _, part := range []string{"photo", "bad"} {
		if _, err := s.Initialize(ctx, uuid.NewString(), u.ID, part); err == nil {
			t.Fatal("cross-account initialization")
		}
	}
}

func TestMySQLLiveMultipartEvidenceFailureAndPurgeGate(t *testing.T) {
	_, s, _, u, p := liveMultipartFixture(t)
	ctx := context.Background()
	p.initiate = func(context.Context, string) (string, error) {
		if _, err := s.repository.db.Exec(`ALTER TABLE album_live_pair_multiparts RENAME COLUMN updated_at TO hidden_updated_at`); err != nil {
			t.Fatal(err)
		}
		return "provider-photo", nil
	}
	_, err := s.Initialize(ctx, u.OwnerID, u.ID, "photo")
	if !errors.Is(err, ErrMultipartPending) {
		t.Fatal("lost durable provider binding reported ready", err)
	}
	if _, err = s.repository.db.Exec(`ALTER TABLE album_live_pair_multiparts RENAME COLUMN hidden_updated_at TO updated_at`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Initialize(ctx, u.OwnerID, u.ID, "photo"); !errors.Is(err, ErrMultipartPending) {
		t.Fatal(err)
	}
	if p.initiations.Load() != 1 {
		t.Fatal("unknown initiation repeated")
	}
	if err = s.Reconcile(ctx, u.OwnerID, u.ID, "photo"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.repository.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = requireNoDirectUploads(ctx, tx, u.OwnerID, u.AssetID)
	tx.Rollback()
	if !errors.Is(err, ErrDerivedPending) {
		t.Fatal("open multipart passed purge", err)
	}
	if _, err = s.Complete(ctx, u.OwnerID, u.ID, "photo"); err != nil {
		t.Fatal(err)
	}
	tx, err = s.repository.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = requireNoDirectUploads(ctx, tx, u.OwnerID, u.AssetID)
	tx.Rollback()
	if err != nil {
		t.Fatal("terminal multipart could not close writer fence", err)
	}
	if _, err = s.repository.db.Exec(`ALTER TABLE album_live_pair_multiparts RENAME TO hidden_live_pair_multiparts`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := s.repository.db.Exec(`ALTER TABLE hidden_live_pair_multiparts RENAME TO album_live_pair_multiparts`); err != nil {
			t.Error(err)
		}
	}()
	if err = s.repository.CheckLivePairSchema(ctx); err == nil {
		t.Fatal("missing migration accepted")
	}
}
