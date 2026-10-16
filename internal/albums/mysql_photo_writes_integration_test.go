package albums

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func photoJournalDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set MYSQL_TEST_DSN for real MySQL photo journal tests")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(config.DBName, "_test") {
		t.Fatal("refusing non-test database")
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = true
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
func photoJournalUpload(t *testing.T, db *sql.DB) (Upload, PhotoCandidate) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := NewMySQLUploads(db)
	_, _, _, owner, m := uploadFixture(t)
	m.Resources[0].SizeBytes = 4096
	digest, _ := m.Digest()
	u := Upload{ID: uuid.NewString(), OwnerID: owner, RequestID: uuid.NewString(), Manifest: m, Digest: digest, State: Uploading, OriginalBytes: 4096, DerivedBudget: 4096, ExpiresAt: now.Add(time.Hour)}
	if err := r.ProvisionAccount(ctx, owner, 16384); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM album_derived_cleanups WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_derived_finalizations WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_photo_writes WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_photo_jobs WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_asset_versions WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_uploads WHERE owner_id=?`, owner)
		db.Exec(`DELETE FROM album_storage_accounts WHERE owner_id=?`, owner)
	})
	if _, err := r.Reserve(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, owner, u.ID, now); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := r.Claim(ctx, owner, u.ID, token, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	resource := m.Resources[0]
	objects := []SealedObject{{ResourceID: resource.ID, Key: sealedKey(u, resource.ID), VersionID: "source-version", SizeBytes: resource.SizeBytes, SHA256: resource.SHA256}}
	var err error
	recordFixtureSeals(t, r, u, token, objects, now)
	u, err = r.Complete(ctx, owner, u.ID, token, objects, now)
	if err != nil {
		t.Fatal(err)
	}
	_, candidate := photoWriteFixture(t)
	return u, candidate
}
func photoJournalSpec(u Upload, c PhotoCandidate, preset PhotoPreset) PhotoWriteSpec {
	return PhotoWriteSpec{OwnerID: u.OwnerID, AttemptID: uuid.NewString(), Preset: preset, SizeBytes: int64(len(c.Bytes)), SHA256: c.SHA256, Width: c.Width, Height: c.Height}
}
func photoJournalReceipt(spec PhotoWriteSpec) SealedObject {
	key, _ := spec.key()
	return SealedObject{ResourceID: string(spec.Preset), Key: key, VersionID: "derived-version", SizeBytes: spec.SizeBytes, SHA256: spec.SHA256}
}
func photoJournalBalance(t *testing.T, db *sql.DB, owner string) (int64, int64) {
	t.Helper()
	var used, reserved int64
	if err := db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	return used, reserved
}

func TestMySQLPhotoWriteOnlyOneWriterAndSingleQuotaTransfer(t *testing.T) {
	db := photoJournalDB(t)
	u, c := photoJournalUpload(t, db)
	ctx := context.Background()
	now := time.Now().UTC()
	var group sync.WaitGroup
	var freshCount atomic.Int32
	records := make(chan PhotoWriteRecord, 20)
	failures := make(chan error, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			r := NewMySQLUploads(db)
			record, fresh, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, photoJournalSpec(u, c, PhotoGridPNG), now)
			if err != nil {
				failures <- err
				return
			}
			if fresh {
				freshCount.Add(1)
			}
			records <- record
		}()
	}
	group.Wait()
	close(records)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if freshCount.Load() != 1 {
		t.Fatal("multiple PUT permissions", freshCount.Load())
	}
	var expected PhotoWriteRecord
	for r := range records {
		if expected.Spec.AttemptID != "" && r != expected {
			t.Fatal("different attempts for one preset")
		}
		expected = r
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != 4096 || reserved != 4096 {
		t.Fatal("consuming attempt changed quota")
	}
	r := NewMySQLUploads(db)
	if _, err := r.PhotoWrite(ctx, uuid.NewString(), u.ID, PhotoGridPNG); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-account record visible", err)
	}
	tooLarge := photoJournalSpec(u, c, PhotoDailyPNG)
	tooLarge.SizeBytes = 4096
	if _, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, tooLarge, now); !errors.Is(err, ErrQuota) {
		t.Fatal("source budget exceeded", err)
	}
	receipt := photoJournalReceipt(expected.Spec)
	failures = make(chan error, 20)
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := NewMySQLUploads(db).CompletePhotoWrite(ctx, expected, receipt, now)
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	used, reserved = photoJournalBalance(t, db, u.OwnerID)
	if used != 4096+expected.Spec.SizeBytes || reserved != 4096-expected.Spec.SizeBytes {
		t.Fatal("quota transferred more than once", used, reserved)
	}
	receipt.VersionID = "different-version"
	if _, err := r.CompletePhotoWrite(ctx, expected, receipt, now); !errors.Is(err, ErrConflict) {
		t.Fatal("receipt overwritten", err)
	}
}

func TestMySQLPhotoWriteQuotaFailureRollsBackReceipt(t *testing.T) {
	db := photoJournalDB(t)
	u, c := photoJournalUpload(t, db)
	ctx := context.Background()
	r := NewMySQLUploads(db)
	now := time.Now().UTC()
	record, _, err := r.BeginPhotoWrite(ctx, u.ID, u.Digest, photoJournalSpec(u, c, PhotoGridPNG), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=0 WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompletePhotoWrite(ctx, record, photoJournalReceipt(record.Spec), now); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	reread, err := r.PhotoWrite(ctx, u.OwnerID, u.ID, PhotoGridPNG)
	if err != nil || reread.ObjectVersion != "" {
		t.Fatal("receipt published without accounting", err)
	}
	used, _ := photoJournalBalance(t, db, u.OwnerID)
	if used != 4096 {
		t.Fatal("partial quota commit")
	}
	if _, err := db.Exec(`UPDATE album_storage_accounts SET reserved_bytes=4096 WHERE owner_id=?`, u.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CompletePhotoWrite(ctx, record, photoJournalReceipt(record.Spec), now); err != nil {
		t.Fatal(err)
	}
}

type journalProvider struct {
	*objectTestStore
	candidate               PhotoCandidate
	renders, puts, recovers int
}

func (p *journalProvider) RenderPhotoCandidate(context.Context, SealedObject, PhotoPreset) (PhotoCandidate, error) {
	p.renders++
	return p.candidate, nil
}
func (p *journalProvider) StorePhotoCandidateOnce(context.Context, PhotoWriteSpec, PhotoCandidate) (SealedObject, error) {
	p.puts++
	return SealedObject{}, ErrPhotoWriteUncertain
}
func (p *journalProvider) ReconcilePhotoCandidate(_ context.Context, spec PhotoWriteSpec) (SealedObject, error) {
	p.recovers++
	return photoJournalReceipt(spec), nil
}
func TestMySQLPhotoDerivativeResumesAcrossServiceAndDatabaseReopen(t *testing.T) {
	db := photoJournalDB(t)
	u, c := photoJournalUpload(t, db)
	ctx := context.Background()
	_, _, objects, _, _ := uploadFixture(t)
	provider := &journalProvider{objectTestStore: objects, candidate: c}
	service, err := NewUploadService(NewMySQLUploads(db), provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GeneratePhotoDerivative(ctx, u.OwnerID, u.ID, PhotoGridPNG); !errors.Is(err, ErrPhotoWriteUncertain) {
		t.Fatal("lost PUT not preserved", err)
	}
	used, reserved := photoJournalBalance(t, db, u.OwnerID)
	if used != 4096 || reserved != 4096 {
		t.Fatal("uncertain write consumed used quota")
	}
	reopened := photoJournalDB(t)
	service, err = NewUploadService(NewMySQLUploads(reopened), provider, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		record, err := service.GeneratePhotoDerivative(ctx, u.OwnerID, u.ID, PhotoGridPNG)
		if err != nil || record.ObjectVersion != "derived-version" {
			t.Fatal("recovery failed", err)
		}
	}
	if provider.renders != 1 || provider.puts != 1 || provider.recovers != 1 {
		t.Fatalf("recovery repeated work: %+v", provider)
	}
	used, reserved = photoJournalBalance(t, db, u.OwnerID)
	if used != 4096+int64(len(c.Bytes)) || reserved != 4096-int64(len(c.Bytes)) {
		t.Fatal("recovery quota", used, reserved)
	}
}

func TestMySQLPhotoPreviewGrantOnlyAfterReceiptAndQuotaCommit(t *testing.T) {
	db := photoJournalDB(t)
	u, c := photoJournalUpload(t, db)
	ctx := context.Background()
	repo := NewMySQLUploads(db)
	_, _, objects, _, _ := uploadFixture(t)
	signer := &previewReadSigner{readSigner: &readSigner{objectTestStore: objects}}
	service, err := NewUploadService(repo, signer, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PhotoPreviewReadGrant(ctx, u.OwnerID, u.ID, PhotoGridPNG); !errors.Is(err, ErrPhotoWriteUncertain) || signer.calls != 0 {
		t.Fatal("preview awaiting rendering was not kept pending", err)
	}
	record, _, err := repo.BeginPhotoWrite(ctx, u.ID, u.Digest, photoJournalSpec(u, c, PhotoGridPNG), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PhotoPreviewReadGrant(ctx, u.OwnerID, u.ID, PhotoGridPNG); !errors.Is(err, ErrPhotoWriteUncertain) || signer.calls != 0 {
		t.Fatal("pending write granted", err)
	}
	if _, err := repo.CompletePhotoWrite(ctx, record, photoJournalReceipt(record.Spec), time.Now()); err != nil {
		t.Fatal(err)
	}
	reopened := photoJournalDB(t)
	service, err = NewUploadService(NewMySQLUploads(reopened), signer, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	beforeUsed, beforeReserved := photoJournalBalance(t, db, u.OwnerID)
	for range 3 {
		grant, err := service.PhotoPreviewReadGrant(ctx, u.OwnerID, u.ID, PhotoGridPNG)
		if err != nil || grant.ObjectVersionID != "derived-version" || grant.ContentType != "image/png" || grant.ReplacementEligible || grant.SizeBytes != record.Spec.SizeBytes {
			t.Fatal("persisted preview grant failed", err)
		}
	}
	afterUsed, afterReserved := photoJournalBalance(t, db, u.OwnerID)
	if beforeUsed != afterUsed || beforeReserved != afterReserved {
		t.Fatal("read changed quota")
	}
	if _, err := service.PhotoPreviewReadGrant(ctx, uuid.NewString(), u.ID, PhotoGridPNG); !errors.Is(err, ErrNotFound) || signer.calls != 3 {
		t.Fatal("cross account read granted", err)
	}
}

// Synthetic receipts exercise the SQL protocol, not real provider COPY evidence.
func recordFixtureSeals(t *testing.T, r *MySQLUploads, u Upload, token string, objects []SealedObject, now time.Time) {
	t.Helper()
	u.LeaseToken = token
	for _, object := range objects {
		if _, fresh, err := r.BeginOriginalSeal(context.Background(), u, object.ResourceID, now); err != nil || !fresh {
			t.Fatal("fixture seal intent", err)
		}
		if err := r.RecordOriginalSeal(context.Background(), u, object.ResourceID, object.VersionID); err != nil {
			t.Fatal(err)
		}
	}
}
