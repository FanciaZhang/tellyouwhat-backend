//go:build albums_live

package albums

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// This probe seeds original metadata only in an isolated local database. It
// tests SQL/provider fencing, not an App backup, entitlement or paid-user journey.
func photoSQLProbeDatabase(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("ALBUM_PHOTO_SQL_COS_PROBE") != "1" {
		t.Skip("explicit isolated SQL and COS photo probe required")
	}
	config, err := mysql.ParseDSN(os.Getenv("MYSQL_TEST_DSN"))
	directory := os.Getenv("ALBUM_PHOTO_SQL_SOCKET_DIRECTORY")
	info, statErr := os.Lstat(directory)
	if err != nil || config.Net != "unix" || !strings.HasPrefix(config.DBName, "albums_photo_probe_") ||
		!strings.HasSuffix(config.DBName, "_test") || directory == "" || filepath.Dir(config.Addr) != directory ||
		statErr != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("dedicated private Unix-socket database required")
	}
	return photoJournalDB(t)
}

// Inventory uses the existing cleanup identity; reads and Abort use the backend
// identity. The worker cannot delete a version or invoke a video/CI operation.
type photoSQLCleanupProvider struct {
	reader, cleaner *COSObjects
	spec            PhotoWriteSpec
}

func (p photoSQLCleanupProvider) ListDerivedVersions(ctx context.Context, target DerivedCleanupTarget) ([]DerivedObjectVersion, error) {
	if target != (DerivedCleanupTarget{"photo", p.spec.OwnerID, p.spec.AttemptID, string(p.spec.Preset)}) {
		return nil, ErrInvalidManifest
	}
	return p.cleaner.ListDerivedVersions(ctx, target)
}
func (photoSQLCleanupProvider) DeleteDerivedVersion(context.Context, DerivedCleanupTarget, DerivedObjectVersion) error {
	return ErrConflict
}
func (p photoSQLCleanupProvider) AssessPhotoCleanup(ctx context.Context, record PhotoWriteRecord, versions []DerivedObjectVersion) (*SealedObject, bool, error) {
	if record.Spec != p.spec {
		return nil, false, ErrInvalidManifest
	}
	return p.reader.AssessPhotoCleanup(ctx, record, versions)
}
func (p photoSQLCleanupProvider) FindPhotoMultipart(ctx context.Context, spec PhotoWriteSpec) (string, error) {
	if spec != p.spec {
		return "", ErrInvalidManifest
	}
	return p.reader.FindPhotoMultipart(ctx, spec)
}
func (p photoSQLCleanupProvider) AbortPhotoMultipart(ctx context.Context, record PhotoWriteRecord) error {
	if record.Spec != p.spec {
		return ErrInvalidManifest
	}
	return p.reader.AbortPhotoMultipart(ctx, record)
}
func (photoSQLCleanupProvider) InspectVideoCleanup(context.Context, VideoJob) (VideoCleanupObservation, error) {
	return VideoCleanupObservation{}, ErrInvalidManifest
}
func (photoSQLCleanupProvider) VerifyVideoResult(context.Context, VideoJob) (VideoResult, error) {
	return VideoResult{}, ErrInvalidManifest
}

func photoSQLProbeState(t *testing.T, folder, name string, repository *MySQLUploads, record PhotoWriteRecord) PhotoWriteRecord {
	t.Helper()
	stored, err := repository.PhotoWriteByAttempt(context.Background(), record.Spec.OwnerID, record.Spec.AttemptID)
	if err != nil {
		t.Fatal("photo SQL evidence unavailable")
	}
	used, reserved := photoJournalBalance(t, repository.db, record.Spec.OwnerID)
	photoMultipartProbeCheckpoint(t, folder, name, map[string]any{"record": stored, "usedBytes": used, "reservedBytes": reserved})
	return stored
}

// At most two tiny synthetic JPEGs. Each writer is held at the production
// pre-Complete callback while the real cleanup worker runs against real MySQL
// and COS. No response, object version, inventory or SQL receipt is substituted.
func TestLiveCOSPhotoSQLCompletionRetirement(t *testing.T) {
	db := photoSQLProbeDatabase(t)
	cleaner := completionProbeCleaner(t)
	reader, root, ctx := liveRoundtripFixture(t)
	completionProbeLease(t, root)
	for _, mode := range []string{"sql-retirement-first", "sql-completion-first"} {
		if !t.Run(mode, func(t *testing.T) {
			folder := filepath.Join(root, mode)
			if os.Mkdir(folder, 0700) != nil {
				t.Fatal("fresh SQL probe folder required; recover retained intent")
			}
			upload, _ := photoJournalUpload(t, db)
			candidate := photoMultipartProbeCandidate(t)
			spec := photoJournalSpec(upload, candidate, PhotoGridJPEG)
			spec.Multipart = true
			repository := NewMySQLUploads(db)
			now := time.Now().UTC().Truncate(time.Microsecond)
			record, fresh, err := repository.BeginPhotoWrite(ctx, upload.ID, upload.Digest, spec, now)
			if err != nil || !fresh {
				t.Fatal("SQL photo intent unavailable")
			}
			intent := photoMultipartProbeIntent{Scope: photoMultipartProbeScope, Spec: spec}
			key, _ := spec.key()
			versions, err := cleaner.listPurgeVersions(ctx, spec.OwnerID, purgeTarget{key})
			uploads, uploadErr := cleaner.listPurgeUploads(ctx, spec.OwnerID, purgeTarget{key})
			if err != nil || uploadErr != nil || len(versions)+len(uploads) != 0 {
				t.Fatal("new SQL probe key not confirmed empty")
			}
			photoMultipartProbeCheckpoint(t, folder, "intent.json", intent)
			trace := photoMultipartProbeTrace(t, reader)
			inventoryTrace := photoMultipartProbeTrace(t, cleaner)
			t.Cleanup(func() {
				roundtripJSON(t, folder, "http.json", trace())
				roundtripJSON(t, folder, "inventory-http.json", inventoryTrace())
			})
			t.Cleanup(func() { cleanPhotoMultipartProbe(t, cleaner, reader, folder, intent) })
			id, err := reader.StartPhotoMultipart(ctx, spec)
			if err != nil {
				t.Fatal("photo initiation unknown; retain intent")
			}
			photoMultipartProbeCheckpoint(t, folder, "session.json", map[string]string{"uploadID": id})
			record, err = repository.BindPhotoMultipart(ctx, record, id, now)
			if err != nil {
				t.Fatal("photo session not durably bound")
			}
			if _, err := db.ExecContext(ctx, `UPDATE album_photo_jobs SET state='attention' WHERE owner_id=?`, upload.OwnerID); err != nil {
				t.Fatal("synthetic job fixture unavailable")
			}
			if _, err := repository.FinalizeDerivedBudget(ctx, DerivedVersionRef{upload.OwnerID, upload.ID}, now); err != nil {
				t.Fatal("photo escrow could not be closed")
			}
			cleanup := derivedCleanupFor(t, repository, upload.OwnerID, "photo", spec.AttemptID)
			photoSQLProbeState(t, folder, "sql-bound.json", repository, record)
			storage := photoSQLCleanupProvider{reader, cleaner, spec}
			worker, err := NewDerivedCleanupWorker(oneDerivedCleanupQueue{repository, cleanup.OwnerID, cleanup.ID}, storage, 1, func() time.Time { return now })
			if err != nil {
				t.Fatal("photo cleanup worker unavailable")
			}

			gate, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			writeCtx, cancel := context.WithCancel(ctx)
			var receipt SealedObject
			var writeErr error
			admissionTime := now
			go func() {
				defer close(finished)
				beforeComplete := func(call context.Context) error {
					if mode == "sql-completion-first" {
						if _, err := repository.PreparePhotoMultipartCompletion(call, record, admissionTime); err != nil {
							return err
						}
					}
					close(gate)
					select {
					case <-release:
					case <-call.Done():
						return call.Err()
					}
					if mode == "sql-retirement-first" {
						_, err := repository.PreparePhotoMultipartCompletion(call, record, admissionTime)
						return err
					}
					return nil
				}
				receipt, writeErr = reader.StorePhotoMultipartOnce(writeCtx, spec, id, candidate, beforeComplete)
			}()
			// Join before any cloud cleanup, including when a main-thread assertion fails.
			t.Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-finished })
			select {
			case <-gate:
			case <-finished:
				t.Fatal("photo writer failed before the SQL completion gate")
			case <-ctx.Done():
				t.Fatal("photo writer gate timed out; retain evidence")
			}
			if mode == "sql-completion-first" {
				photoMultipartProbeCheckpoint(t, folder, "completion-request.json", map[string]bool{"mayComplete": true})
			}
			batch, err := worker.RunOnce(ctx)
			if err != nil || batch.Failed+batch.Contended != 0 {
				t.Fatal("photo cleanup did not reach the controlled fence")
			}
			stored := photoSQLProbeState(t, folder, "sql-fenced.json", repository, record)
			used, reserved := photoJournalBalance(t, db, upload.OwnerID)
			if mode == "sql-retirement-first" {
				if batch.Retired != 1 || stored.MultipartPhase != "aborted" || stored.RetiredAt.IsZero() || used != upload.OriginalBytes || reserved != 0 {
					t.Fatal("retirement did not fence completion and settle escrow")
				}
			} else if batch.Waiting != 1 || stored.MultipartPhase != "completing" || !stored.RetirementStartedAt.IsZero() || used != upload.OriginalBytes || reserved != spec.SizeBytes {
				t.Fatal("admitted completion was aborted or refunded before a receipt")
			}
			releaseOnce.Do(func() { close(release) })
			<-finished
			if mode == "sql-retirement-first" {
				if !errors.Is(writeErr, ErrDerivedRetired) {
					t.Fatal("retired writer did not stop before Complete")
				}
			} else {
				if writeErr != nil || !concreteCOSVersion(receipt.VersionID) {
					t.Fatal("photo completion unresolved; inspect retained HTTP trace")
				}
				photoMultipartProbeCheckpoint(t, folder, "receipt.json", receipt)
				stored = photoSQLProbeState(t, folder, "sql-before-recovery.json", repository, record)
				if stored.ObjectVersion != "" {
					t.Fatal("fixture unexpectedly saved the writer receipt")
				}
			}
			// A new connection and worker must observe the same session and real object.
			repository = NewMySQLUploads(photoSQLProbeDatabase(t))
			now = now.Add(16 * time.Minute)
			worker, err = NewDerivedCleanupWorker(oneDerivedCleanupQueue{repository, cleanup.OwnerID, cleanup.ID}, storage, 1, func() time.Time { return now })
			if err != nil {
				t.Fatal("recovery worker unavailable")
			}
			batch, err = worker.RunOnce(ctx)
			if err != nil || batch.Failed+batch.Waiting+batch.Contended != 0 {
				t.Fatal("SQL/COS recovery failed")
			}
			if mode == "sql-completion-first" {
				if batch.Recovered != 1 {
					t.Fatal("concrete photo receipt was not recovered")
				}
				batch, err = worker.RunOnce(ctx)
				if err != nil || batch.Pruned != 1 || batch.Failed+batch.Waiting+batch.Contended != 0 {
					t.Fatal("recovered version was not kept")
				}
			}
			stored = photoSQLProbeState(t, folder, "sql-recovered.json", repository, record)
			used, reserved = photoJournalBalance(t, repository.db, upload.OwnerID)
			wantUsed := upload.OriginalBytes
			if mode == "sql-completion-first" {
				wantUsed += spec.SizeBytes
				if stored.ObjectVersion != receipt.VersionID || !stored.RetiredAt.IsZero() {
					t.Fatal("recovery changed the completed object identity")
				}
			}
			if used != wantUsed || reserved != 0 {
				t.Fatal("recovery settled quota incorrectly")
			}
			batch, err = worker.RunOnce(ctx)
			if err != nil || batch != (DerivedCleanupBatch{}) {
				t.Fatal("settled cleanup was replayed")
			}
			completions, aborts := 0, 0
			for _, entry := range trace() {
				if entry["multipart"] == true {
					if entry["method"] == "POST" {
						completions++
					}
					if entry["method"] == "DELETE" {
						aborts++
					}
				}
			}
			if mode == "sql-retirement-first" && (completions != 0 || aborts != 1) ||
				mode == "sql-completion-first" && (completions != 1 || aborts != 0) {
				t.Fatal("SQL gate allowed incompatible provider operations")
			}
			photoMultipartProbeCheckpoint(t, folder, "result.json", map[string]any{"passed": true, "completions": completions, "aborts": aborts, "usedBytes": used, "reservedBytes": reserved, "connectionReopened": true, "appBackupVerified": false})
		}) {
			return
		}
	}
}

func TestPhotoSQLProbeRejectsForeignOperations(t *testing.T) {
	spec, _ := photoWriteFixture(t)
	forbidden := cosFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("forbidden SQL probe operation reached COS")
		return nil, nil
	})
	p := photoSQLCleanupProvider{forbidden, forbidden, spec}
	wrong := spec
	wrong.SHA256 = strings.Repeat("a", 64)
	if _, err := p.FindPhotoMultipart(context.Background(), wrong); err == nil {
		t.Fatal("foreign discovery accepted")
	}
	if err := p.AbortPhotoMultipart(context.Background(), PhotoWriteRecord{Spec: wrong}); err == nil {
		t.Fatal("foreign abort accepted")
	}
	if _, _, err := p.AssessPhotoCleanup(context.Background(), PhotoWriteRecord{Spec: wrong}, nil); err == nil {
		t.Fatal("foreign assessment accepted")
	}
	if _, err := p.ListDerivedVersions(context.Background(), DerivedCleanupTarget{}); err == nil {
		t.Fatal("foreign inventory accepted")
	}
	if err := p.DeleteDerivedVersion(context.Background(), DerivedCleanupTarget{"photo", spec.OwnerID, spec.AttemptID, string(spec.Preset)}, DerivedObjectVersion{}); err == nil {
		t.Fatal("version deletion accepted")
	}
	if _, err := p.InspectVideoCleanup(context.Background(), VideoJob{}); err == nil {
		t.Fatal("CI inspection accepted")
	}
	if _, err := p.VerifyVideoResult(context.Background(), VideoJob{}); err == nil {
		t.Fatal("CI verification accepted")
	}
}
