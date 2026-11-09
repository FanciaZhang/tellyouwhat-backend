package albums

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type multipartPhotoRenderer struct {
	*COSObjects
	candidate  PhotoCandidate
	renders    int
	afterStart func()
}

func (p *multipartPhotoRenderer) StartPhotoMultipart(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	id, err := p.COSObjects.StartPhotoMultipart(ctx, spec)
	if err == nil && p.afterStart != nil {
		p.afterStart()
	}
	return id, err
}

func (p *multipartPhotoRenderer) RenderPhotoCandidate(context.Context, SealedObject, PhotoPreset) (PhotoCandidate, error) {
	p.renders++
	return p.candidate, nil
}

func TestMySQLPhotoMultipartPersistsBeforeBytesAndResumesAcrossRepositories(t *testing.T) {
	for _, fault := range []string{"init-lost", "part-not-sent", "part-lost", "complete-lost"} {
		t.Run(fault, func(t *testing.T) {
			db := photoJournalDB(t)
			u, candidate := photoJournalUpload(t, db)
			r := NewMySQLUploads(db)
			// The generated attempt ID is read from the persisted intent during initiation.
			var actual *COSObjects
			var protocol *photoMultipartProtocol
			var provider *multipartPhotoRenderer
			bridge := cosFixture(t, func(request *http.Request) (*http.Response, error) {
				if actual == nil {
					record, err := r.PhotoWrite(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
					if err != nil || !record.Spec.Multipart || record.MultipartID != "" {
						t.Fatal("provider initiation preceded durable intent", record, err)
					}
					actual, protocol = photoMultipartTransport(t, record.Spec, candidate, fault)
					protocol.beforePart = func() {
						bound, err := r.PhotoWrite(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
						if err != nil || bound.MultipartID != protocol.id {
							t.Fatal("photo bytes preceded committed session binding", bound, err)
						}
					}
					protocol.beforeCompletion = func() {
						bound, err := r.PhotoWrite(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
						if err != nil || bound.MultipartID != protocol.id || bound.MultipartPhase != "completing" {
							t.Fatal("Complete preceded committed admission", bound, err)
						}
					}
				}
				return actual.archiveHTTP.Transport.RoundTrip(request)
			})
			provider = &multipartPhotoRenderer{COSObjects: bridge, candidate: candidate}
			service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = service.GeneratePhotoDerivative(context.Background(), u.OwnerID, u.ID, PhotoGridPNG); !errors.Is(err, ErrPhotoWriteUncertain) {
				t.Fatal("lost response not preserved", err)
			}
			used, reserved := photoJournalBalance(t, db, u.OwnerID)
			if used != 4096 || reserved != 4096 {
				t.Fatal("unknown write charged quota", used, reserved)
			}
			service, err = NewUploadService(NewMySQLUploads(photoJournalDB(t)), provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				record, err := service.GeneratePhotoDerivative(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
				if err != nil || record.MultipartID != protocol.id || record.ObjectVersion != "photo-version+fixed" {
					t.Fatal("photo recovery failed", record, err)
				}
			}
			if protocol.initiations != 1 || protocol.completions != 1 {
				t.Fatal("recovery recreated upload or object", protocol)
			}
			wantRenders := 1
			if fault == "part-not-sent" || fault == "init-lost" {
				wantRenders = 2
			}
			if provider.renders != wantRenders {
				t.Fatal("unexpected image regeneration", provider.renders)
			}
			used, reserved = photoJournalBalance(t, db, u.OwnerID)
			if used != 4096+int64(len(candidate.Bytes)) || reserved != 4096-int64(len(candidate.Bytes)) {
				t.Fatal("recovery charged twice", used, reserved)
			}
		})
	}
}

func TestMySQLPhotoMultipartBindingFences(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	now := time.Now().UTC()
	ctx := context.Background()
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	spec.Multipart = true
	record, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, spec, now)
	if err != nil || !fresh {
		t.Fatal(record, fresh, err)
	}
	if _, err = r.CompletePhotoWrite(ctx, record, photoJournalReceipt(spec), now); !errors.Is(err, ErrConflict) {
		t.Fatal("unbound multipart receipt published", err)
	}
	for _, mutate := range []func(*PhotoWriteRecord){
		func(v *PhotoWriteRecord) { v.Spec.OwnerID = uuid.NewString() },
		func(v *PhotoWriteRecord) { v.Spec.SHA256 = strings.Repeat("a", 64) },
		func(v *PhotoWriteRecord) { v.VersionID = uuid.NewString() },
		func(v *PhotoWriteRecord) { v.ManifestDigest = strings.Repeat("a", 64) },
	} {
		wrong := record
		mutate(&wrong)
		if _, err = r.BindPhotoMultipart(ctx, wrong, "session-one", now); err == nil {
			t.Fatal("foreign descriptor bound")
		}
	}
	bound, err := r.BindPhotoMultipart(ctx, record, "session-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.BindPhotoMultipart(ctx, record, "session-other", now); !errors.Is(err, ErrConflict) {
		t.Fatal("bound session replaced", err)
	}
	if _, err = r.CompletePhotoWrite(ctx, bound, photoJournalReceipt(spec), now); !errors.Is(err, ErrConflict) {
		t.Fatal("unadmitted completion published", err)
	}
	bound, err = r.PreparePhotoMultipartCompletion(ctx, bound, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.CompletePhotoWrite(ctx, bound, photoJournalReceipt(spec), now); err != nil {
		t.Fatal(err)
	}
	if _, err = r.BindPhotoMultipart(ctx, record, "session-one", now); err != nil {
		t.Fatal("idempotent observation lost", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE album_photo_writes SET multipart=FALSE WHERE attempt_id=?`, spec.AttemptID); err == nil {
		t.Fatal("schema allowed direct record with multipart handle")
	}
}

func TestMySQLPhotoMultipartKeepsLegacyRecoveryReadOnly(t *testing.T) {
	db := photoJournalDB(t)
	u, candidate := photoJournalUpload(t, db)
	r := NewMySQLUploads(db)
	spec := photoJournalSpec(u, candidate, PhotoGridPNG)
	if _, fresh, err := r.BeginPhotoWrite(context.Background(), u.ID, u.Digest, spec, time.Now()); err != nil || !fresh {
		t.Fatal("legacy fixture", fresh, err)
	}
	storage, protocol := photoMultipartTransport(t, spec, candidate, "")
	protocol.committed = true
	provider := &multipartPhotoRenderer{COSObjects: storage, candidate: candidate}
	service, err := NewUploadService(r, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.GeneratePhotoDerivative(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
	if err != nil || result.Spec.Multipart || result.MultipartID != "" || result.ObjectVersion != "photo-version+fixed" || provider.renders != 0 || protocol.initiations+protocol.puts+protocol.completions != 0 {
		t.Fatal("legacy write was changed", result, err, protocol)
	}
}

type interruptedPhotoBinding struct {
	*MySQLUploads
	fault  string
	cancel context.CancelFunc
}

func (r *interruptedPhotoBinding) BindPhotoMultipart(ctx context.Context, record PhotoWriteRecord, id string, now time.Time) (PhotoWriteRecord, error) {
	bound, err := r.MySQLUploads.BindPhotoMultipart(ctx, record, id, now)
	if err != nil {
		return bound, err
	}
	switch r.fault {
	case "cancel":
		r.cancel()
	case "lost-reply":
		return PhotoWriteRecord{}, ErrPhotoWriteUncertain
	case "retired":
		if _, err := r.db.ExecContext(ctx, `UPDATE album_photo_writes SET retirement_started_at=? WHERE attempt_id=?`, now, record.Spec.AttemptID); err != nil {
			return PhotoWriteRecord{}, err
		}
		return r.PhotoWrite(ctx, record.Spec.OwnerID, record.VersionID, record.Spec.Preset)
	}
	return bound, nil
}

func TestMySQLPhotoMultipartBindingInterruptionsKeepSessionWithoutSendingBytes(t *testing.T) {
	for _, fault := range []string{"cancel", "cancel-before-bind", "lost-reply", "retired"} {
		t.Run(fault, func(t *testing.T) {
			db := photoJournalDB(t)
			u, candidate := photoJournalUpload(t, db)
			base := NewMySQLUploads(db)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := &interruptedPhotoBinding{MySQLUploads: base, fault: fault, cancel: cancel}
			var actual *COSObjects
			var protocol *photoMultipartProtocol
			bridge := cosFixture(t, func(request *http.Request) (*http.Response, error) {
				if actual == nil {
					record, err := base.PhotoWrite(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
					if err != nil {
						t.Fatal(err)
					}
					actual, protocol = photoMultipartTransport(t, record.Spec, candidate, "")
				}
				return actual.archiveHTTP.Transport.RoundTrip(request)
			})
			provider := &multipartPhotoRenderer{COSObjects: bridge, candidate: candidate}
			if fault == "cancel-before-bind" {
				provider.afterStart = cancel
			}
			service, err := NewUploadService(repo, provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = service.GeneratePhotoDerivative(ctx, u.OwnerID, u.ID, PhotoGridPNG); err == nil {
				t.Fatal("interrupted binding sent photo")
			}
			stored, err := base.PhotoWrite(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
			if err != nil || stored.MultipartID != protocol.id || protocol.initiations != 1 || protocol.puts+protocol.completions != 0 {
				t.Fatal("session lost or bytes sent", stored, err, protocol)
			}
			service, err = NewUploadService(NewMySQLUploads(photoJournalDB(t)), provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.GeneratePhotoDerivative(context.Background(), u.OwnerID, u.ID, PhotoGridPNG)
			if fault == "retired" {
				if !errors.Is(err, ErrDerivedRetired) || protocol.puts+protocol.completions != 0 {
					t.Fatal("retired write resumed", err)
				}
			} else if err != nil || protocol.initiations != 1 || protocol.puts != 1 || protocol.completions != 1 {
				t.Fatal("interrupted binding did not resume same session", err, protocol)
			}
		})
	}
}
