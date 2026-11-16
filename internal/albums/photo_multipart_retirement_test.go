package albums

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPhotoMultipartAbortRequiresFenceAndPositiveAbsence(t *testing.T) {
	for _, fault := range []string{"", "already-absent", "lost-reply", "forbidden", "still-present", "wrong-code", "unfenced", "completing", "unknown", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			spec, _ := photoWriteFixture(t)
			spec.Multipart = true
			p := PhotoWriteRecord{Spec: spec, MultipartID: "retirement-session", MultipartPhase: "ready", RetirementStartedAt: time.Now()}
			key, _ := spec.key()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "unfenced":
				p.RetirementStartedAt = time.Time{}
			case "completing", "unknown":
				p.MultipartPhase = fault
			case "cancelled":
				cancel()
			}
			calls := 0
			s := cosFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/"+key || r.URL.Query().Get("uploadId") != p.MultipartID || len(r.URL.Query()) != 1 {
					t.Fatal("unscoped abort", r.URL)
				}
				if fault == "lost-reply" {
					return nil, io.ErrUnexpectedEOF
				}
				response := cosResponse(r, "", "")
				if r.Method == "DELETE" && fault != "already-absent" && fault != "forbidden" {
					response.StatusCode = http.StatusNoContent
					return response, nil
				}
				if fault == "still-present" {
					return cosResponse(r, `<ListPartsResult/>`, ""), nil
				}
				code, status := "NoSuchUpload", http.StatusNotFound
				if fault == "wrong-code" {
					code = "NoSuchKey"
				} else if fault == "forbidden" {
					code, status = "AccessDenied", http.StatusForbidden
				}
				response = cosResponse(r, fmt.Sprintf(`<Error><Code>%s</Code></Error>`, code), "")
				response.StatusCode = status
				return response, nil
			})
			err := s.AbortPhotoMultipart(ctx, p)
			wantSuccess := fault == "" || fault == "already-absent"
			if (err == nil) != wantSuccess {
				t.Fatal("abort observation was misclassified", calls, err)
			}
			if (fault == "unfenced" || fault == "completing" || fault == "unknown" || fault == "cancelled") && calls != 0 {
				t.Fatal("unsafe abort reached provider", calls)
			}
		})
	}
}

func photoRetirementFixture(t *testing.T, phase string, bind bool) (*MySQLUploads, Upload, PhotoWriteRecord, DerivedCleanup, time.Time) {
	t.Helper()
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	spec.Multipart = true
	p, _, err := r.BeginPhotoWrite(context.Background(), u.ID, u.Digest, spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if bind {
		p, err = r.BindPhotoMultipart(context.Background(), p, "retirement-session", now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if phase != "ready" {
		if _, err := db.Exec(`UPDATE album_photo_writes SET multipart_phase=? WHERE attempt_id=?`, phase, p.Spec.AttemptID); err != nil {
			t.Fatal(err)
		}
		p.MultipartPhase = phase
	}
	if _, err := db.Exec(`UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeDerivedBudget(context.Background(), DerivedVersionRef{u.OwnerID, u.ID}, now); err != nil {
		t.Fatal(err)
	}
	return r, u, p, derivedCleanupFor(t, r, u.OwnerID, "photo", p.Spec.AttemptID), now
}

type photoRetirementProvider struct {
	*cleanupProviderFixture
	cos *COSObjects
}

func (p *photoRetirementProvider) FindPhotoMultipart(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	return p.cos.FindPhotoMultipart(ctx, spec)
}
func (p *photoRetirementProvider) AbortPhotoMultipart(ctx context.Context, record PhotoWriteRecord) error {
	return p.cos.AbortPhotoMultipart(ctx, record)
}

func TestMySQLPhotoMultipartRetirementResumesAbortAndRefundsOnce(t *testing.T) {
	for _, fault := range []string{"", "lost-init-reply", "lost-abort-reply", "lost-confirm-reply", "lease-expired", "unexpected-object"} {
		t.Run(fault, func(t *testing.T) {
			r, u, p, c, now := photoRetirementFixture(t, "ready", fault != "lost-init-reply")
			if fault == "" {
				now = now.Add(789 * time.Nanosecond)
			}
			key, _ := p.Spec.key()
			aborts, discoveries := 0, 0
			closed := false
			provider := &photoRetirementProvider{cleanupProviderFixture: &cleanupProviderFixture{}}
			provider.cos = cosFixture(t, func(req *http.Request) (*http.Response, error) {
				q := req.URL.Query()
				if req.Method == "GET" && req.URL.Path == "/" {
					discoveries++
					if q.Get("prefix") != key {
						t.Fatal("unscoped photo discovery")
					}
					return cosResponse(req, `<ListMultipartUploadsResult><Bucket>albums-test-1250000000</Bucket><MaxUploads>1000</MaxUploads><Prefix>`+key+`</Prefix><IsTruncated>false</IsTruncated><Upload><Key>`+key+`</Key><UploadId>retirement-session</UploadId></Upload></ListMultipartUploadsResult>`, ""), nil
				}
				if req.URL.Path != "/"+key || q.Get("uploadId") != "retirement-session" || len(q) != 1 {
					t.Fatal("wrong retirement target", req.Method, req.URL)
				}
				if req.Method == "DELETE" {
					aborts++
					stored, err := r.PhotoWriteByAttempt(context.Background(), u.OwnerID, p.Spec.AttemptID)
					if err != nil || stored.RetirementStartedAt.IsZero() || stored.MultipartPhase != "ready" || stored.MultipartID != "retirement-session" {
						t.Fatal("abort preceded durable fence", stored, err)
					}
					if !closed {
						closed = true
						if fault == "lost-abort-reply" {
							return nil, io.ErrUnexpectedEOF
						}
						response := cosResponse(req, "", "")
						response.StatusCode = http.StatusNoContent
						return response, nil
					}
				} else if req.Method != "GET" || !closed {
					t.Fatal("retirement initiated an unexpected write", req.Method)
				}
				if fault == "lease-expired" && aborts == 1 {
					now = now.Add(11 * time.Minute)
				}
				response := cosResponse(req, `<Error><Code>NoSuchUpload</Code></Error>`, "")
				response.StatusCode = http.StatusNotFound
				return response, nil
			})
			if fault == "unexpected-object" {
				provider.versions = []DerivedObjectVersion{{"unexpected-version", false, 10}}
			}
			var repo DerivedCleanupRepository = oneDerivedCleanupQueue{r, c.OwnerID, c.ID}
			if fault == "lost-confirm-reply" {
				repo = &lostPhotoAbortConfirmation{DerivedCleanupRepository: repo, lose: true}
			}
			w, err := NewDerivedCleanupWorker(repo, provider, 1, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			batch, err := w.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if fault == "unexpected-object" {
				_, reserved := photoJournalBalance(t, r.db, u.OwnerID)
				stored, e := r.PhotoWriteByAttempt(context.Background(), u.OwnerID, p.Spec.AttemptID)
				if batch.Failed != 1 || reserved != p.Spec.SizeBytes || len(provider.deletes) != 0 || !stored.RetiredAt.IsZero() || e != nil {
					t.Fatal("unexpected object was erased or refunded", batch, reserved, stored, e)
				}
				return
			}
			if fault == "lost-abort-reply" || fault == "lost-confirm-reply" || fault == "lease-expired" {
				if batch.Retired != 0 {
					t.Fatal("uncertain retirement was accepted", batch)
				}
				_, reserved := photoJournalBalance(t, r.db, u.OwnerID)
				if reserved != p.Spec.SizeBytes {
					t.Fatal("uncertain abort released escrow", reserved)
				}
				now = now.Add(16 * time.Minute)
				r = NewMySQLUploads(photoJournalDB(t))
				w, err = NewDerivedCleanupWorker(oneDerivedCleanupQueue{r, c.OwnerID, c.ID}, provider, 1, func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				batch, err = w.RunOnce(context.Background())
			}
			stored, e := r.PhotoWriteByAttempt(context.Background(), u.OwnerID, p.Spec.AttemptID)
			used, reserved := photoJournalBalance(t, r.db, u.OwnerID)
			if err != nil || batch.Retired != 1 || e != nil || stored.MultipartPhase != "aborted" || stored.RetiredAt.IsZero() || used != u.OriginalBytes || reserved != 0 || len(provider.deletes) != 0 {
				t.Fatal("photo retirement not settled", batch, stored, used, reserved, err, e)
			}
			if _, err := r.PreparePhotoMultipartCompletion(context.Background(), stored, now); !errors.Is(err, ErrDerivedRetired) {
				t.Fatal("aborted upload re-entered completion", err)
			}
			if _, err := r.CompletePhotoWrite(context.Background(), stored, photoJournalReceipt(p.Spec), now); !errors.Is(err, ErrDerivedRetired) {
				t.Fatal("aborted upload published late receipt", err)
			}
			if batch, err = w.RunOnce(context.Background()); err != nil || batch.Retired != 0 {
				t.Fatal("retirement repeated", batch, err)
			}
			_, reserved = photoJournalBalance(t, r.db, u.OwnerID)
			if reserved != 0 || aborts < 1 || discoveries != map[bool]int{true: 1, false: 0}[fault == "lost-init-reply"] {
				t.Fatal("duplicate refund or unbounded discovery", reserved, aborts, discoveries)
			}
		})
	}
}

func TestPhotoMultipartReceiptRejectsUnadmittedPhases(t *testing.T) {
	spec, _ := photoWriteFixture(t)
	spec.Multipart = true
	for _, phase := range []string{"", "ready", "aborted", "other", "completing", "unknown"} {
		p := PhotoWriteRecord{Spec: spec, MultipartID: "receipt-session", MultipartPhase: phase, ObjectVersion: "committed-version"}
		_, err := p.Receipt()
		if (err == nil) != (phase == "completing" || phase == "unknown") {
			t.Fatal("receipt ignored completion phase", phase, err)
		}
	}
}

type lostPhotoAbortConfirmation struct {
	DerivedCleanupRepository
	lose bool
}

func (r *lostPhotoAbortConfirmation) ConfirmCleanupPhotoMultipartAborted(ctx context.Context, c DerivedCleanup, p PhotoWriteRecord, now time.Time) (PhotoWriteRecord, error) {
	stored, err := r.DerivedCleanupRepository.ConfirmCleanupPhotoMultipartAborted(ctx, c, p, now)
	if err == nil && r.lose {
		r.lose = false
		return PhotoWriteRecord{}, io.ErrUnexpectedEOF
	}
	return stored, err
}

func TestMySQLPhotoMultipartRetirementNeverAbortsUnknownOrCompleting(t *testing.T) {
	for _, phase := range []string{"unknown", "completing"} {
		t.Run(phase, func(t *testing.T) {
			r, u, p, c, now := photoRetirementFixture(t, phase, true)
			storage := &photoRetirementProvider{cleanupProviderFixture: &cleanupProviderFixture{}}
			storage.cos = cosFixture(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("uncertain completion was aborted")
				return nil, nil
			})
			w, err := NewDerivedCleanupWorker(oneDerivedCleanupQueue{r, c.OwnerID, c.ID}, storage, 1, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			b, err := w.RunOnce(context.Background())
			stored, e := r.PhotoWriteByAttempt(context.Background(), u.OwnerID, p.Spec.AttemptID)
			_, reserved := photoJournalBalance(t, r.db, u.OwnerID)
			if err != nil || b.Waiting != 1 || e != nil || !stored.RetirementStartedAt.IsZero() || reserved != p.Spec.SizeBytes {
				t.Fatal("unknown write lost its fence or escrow", b, stored, reserved, err, e)
			}
		})
	}
}

func TestMySQLPhotoMultipartCompletionAndRetirementHaveOneWinner(t *testing.T) {
	for range 12 {
		r, _, p, c, now := photoRetirementFixture(t, "ready", true)
		c, err := r.ClaimDerivedCleanup(context.Background(), c.OwnerID, c.ID, uuid.NewString(), now)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var completeErr, retireErr error
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			_, completeErr = r.PreparePhotoMultipartCompletion(context.Background(), p, now)
		}()
		go func() {
			defer group.Done()
			<-start
			_, retireErr = r.FenceCleanupPhotoMultipart(context.Background(), c, p, now)
		}()
		close(start)
		group.Wait()
		if completeErr == nil {
			if !errors.Is(retireErr, ErrDerivedPending) {
				t.Fatal("completion did not exclude abort", completeErr, retireErr)
			}
		} else if retireErr != nil || !errors.Is(completeErr, ErrDerivedRetired) {
			t.Fatal("retirement did not exclude completion", completeErr, retireErr)
		}
	}
}
