package albums

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// Exercises the real expiry worker and the authenticated restore implementation;
// Apple status is signed fixture data and no cloud object access is authorized.
func TestMySQLRetentionCollectionRecoveryPipeline(t *testing.T) {
	root, binaries := os.Getenv("ALBUM_RETENTION_RECOVERY_ROOT"), os.Getenv("ALBUM_BACKUP_MYSQL_BIN")
	if root == "" || binaries == "" {
		t.Skip("explicit private retention recovery fixture required")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !strings.HasPrefix(filepath.Base(root), "albums-retention-") {
		t.Fatal("refusing an unowned fixture directory")
	}
	marker, err := os.ReadFile(filepath.Join(root, "fixture-owner.json"))
	if err != nil || string(marker) != "albums-retention-recovery-v1\n" {
		t.Fatal("missing explicit fixture ownership")
	}
	for _, scenario := range []string{"expired", "renewed", "provider-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			f := newSharingFixture(t)
			cfg, err := mysql.ParseDSN(f.dsn)
			if err != nil || cfg.Net != "unix" || !strings.HasSuffix(cfg.DBName, "_test") || !strings.HasPrefix(cfg.Addr, root+string(os.PathSeparator)) {
				t.Fatal("refusing a non-fixture source database")
			}
			// The restored MySQL socket remains below macOS's 103-byte path limit.
			folder, err := os.MkdirTemp(root, "r-")
			if err != nil {
				t.Fatal(err)
			}
			// The outer fixture owns these synthetic recovery artifacts, including
			// failure evidence; it stops its services before reclaiming the root.
			settings := fmt.Sprintf("MYSQL_DATABASE=%s\nMYSQL_SOCKET=%s\nMYSQL_USER=%s\nMYSQL_PASSWORD=%s\nALBUM_BACKUP_ENCRYPTION_KEY=%s\n", cfg.DBName, cfg.Addr, cfg.User, cfg.Passwd, strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""))
			if err := os.WriteFile(filepath.Join(folder, "backup.env"), []byte(settings), 0600); err != nil {
				t.Fatal(err)
			}
			owner, control := f.login.AccountID, uuid.NewString()
			first, _ := createTestCollection(t, f)
			second, _ := createTestCollection(t, f)
			for _, id := range []string{first, second} {
				if _, err := f.repo.SaveCollectionMember(ctx, owner, id, f.version.AssetID, CloudCollectionMemberInput{uuid.NewString(), 0, true}, *f.now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.db.Exec(`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,SHA2(?,256))`, control, control); err != nil {
				t.Fatal(err)
			}
			controlCollection := uuid.NewString()
			if _, err := f.db.Exec(`INSERT INTO album_collections(owner_id,collection_id,revision,request_id,name,deleted,updated_at) VALUES(?,?,1,UUID(),'control collection',FALSE,?)`, control, controlCollection, *f.now); err != nil {
				t.Fatal(err)
			}
			used, reserved := photoJournalBalance(t, f.db, owner)
			request, err := json.Marshal(map[string]any{"scenario": scenario, "owner": owner, "collections": []string{first, second}, "control_owner": control, "control_collection": controlCollection,
				"version_id": f.version.VersionID, "manifest_digest": f.version.ManifestDigest, "used_bytes": used, "reserved_bytes": reserved, "expired": scenario == "expired"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(folder, "request.json"), request, 0600); err != nil {
				t.Fatal(err)
			}
			driver, err := filepath.Abs("../../deploy/albums/retention_recovery_integration.py")
			if err != nil {
				t.Fatal(err)
			}
			run := func(phase string) {
				t.Helper()
				command := exec.CommandContext(ctx, "python3", driver, phase, folder, binaries)
				command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
				command.WaitDelay = 45 * time.Second
				command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
				out, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("%s recovery phase failed: %v\n%s", phase, err, out)
				}
				t.Logf("%s: %s", phase, out)
			}
			run("prepare")
			var original, transaction string
			if err := f.db.QueryRow(`SELECT original_transaction_id,transaction_id FROM album_cloud_subscriptions WHERE owner_id=?`, owner).Scan(&original, &transaction); err != nil {
				t.Fatal(err)
			}
			*f.now = f.now.Add(time.Second)
			v := VerifiedAlbumSubscription{original, transaction, Cloud50GBAnnualProduct, "Sandbox", f.now.AddDate(0, -7, 0), *f.now, false}
			applyReminderSubscription(t, f.repo, owner, v, *f.now)
			service := cleanupSubscriptionService(t, f.repo, owner, f.now, func(context.Context) (VerifiedAlbumSubscription, error) {
				if scenario == "provider-unavailable" {
					return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
				}
				if scenario == "renewed" {
					renewed := v
					renewed.Active, renewed.TransactionID, renewed.ExpiresAt = true, "renewed-transaction", f.now.AddDate(1, 0, 0)
					return renewed, nil
				}
				return v, nil
			})
			storage := &purgeStorageFixture{t: t, beforeList: func() { t.Fatal("unclosed media source reached cloud inventory") }}
			worker := &retentionCleanupWorker{repository: f.repo, subscriptions: service, storage: storage, batchSize: 10, now: func() time.Time { return *f.now }}
			batch, err := worker.runOnce(ctx)
			want := 0
			if scenario == "expired" {
				want = 2
			}
			if err != nil || batch.Collections != want || batch.Advanced != 0 || batch.Settled != 0 || len(storage.deletes)+len(storage.aborts) != 0 {
				t.Fatal("unexpected expiry or cloud mutation", batch, err)
			}
			version, err := f.repo.AssetVersion(ctx, owner, f.version.VersionID)
			if err != nil || !reflect.DeepEqual(version, f.version) {
				t.Fatal("original metadata changed", err)
			}
			afterUsed, afterReserved := photoJournalBalance(t, f.db, owner)
			if used != afterUsed || reserved != afterReserved || f.signer.calls.Load() != 0 {
				t.Fatal("media quota or grants changed")
			}
			run("recover")
		})
	}
}
