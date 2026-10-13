package albums

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestMySQLAlbumsRecoveryEncryptedSnapshotAndQuarantinedDrill(t *testing.T) {
	binaries := os.Getenv("ALBUM_BACKUP_TEST_MYSQL_BIN")
	if binaries == "" {
		t.Skip("set ALBUM_BACKUP_TEST_MYSQL_BIN for disposable native MySQL restore drill")
	}
	f := newSharingFixture(t)
	f.create(t, "private-share-fixture", 2)
	// Include a durable deletion intent, unfinished provider revocation and an
	// immutable media version; no cloud request or real user content is involved.
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := NewMySQLAccounts(f.db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	other, err := accounts.RegisterAppleLogin(context.Background(), AppleGrant{Identity: freshAppleIdentity("recovery-deleting-fixture"), RefreshToken: "synthetic-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := NewMySQLSessions(f.db, nil)
	session, err := sessions.Issue(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = accounts.requestAccountDeletion(context.Background(), session.Token, "10000000-0000-4000-8000-000000000099", sessions.now); err != nil {
		t.Fatal(err)
	}
	config, err := mysql.ParseDSN(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.Net != "unix" {
		t.Fatal("backup integration requires isolated unix-socket source")
	}
	root, err := os.MkdirTemp("/tmp", "albums-dr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	envPath := filepath.Join(root, "backup.env")
	env := "MYSQL_SOCKET=" + config.Addr + "\nMYSQL_DATABASE=" + config.DBName + "\nMYSQL_USER=" + config.User + "\nMYSQL_PASSWORD=" + config.Passwd + "\nALBUM_BACKUP_ENCRYPTION_KEY=" + strings.Repeat("fixture-key-", 6) + "\n"
	if err = os.WriteFile(envPath, []byte(env), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../deploy/albums/backup.py")
	if err != nil {
		t.Fatal(err)
	}
	run := func(operation string, extra ...string) map[string]any {
		t.Helper()
		args := []string{script, operation, "--backend-dir", root, "--environment-file", envPath, "--backup-dir", filepath.Join(root, "vault"), "--mysql-bin-dir", binaries}
		args = append(args, extra...)
		data, err := exec.Command("python3", args...).CombinedOutput()
		if err != nil {
			if diagnostic, readErr := os.ReadFile(filepath.Join(root, ".operations", "albums-restore-deletion-fences.stderr")); readErr == nil {
				t.Logf("isolated synthetic fixture diagnostic: %s", diagnostic)
			}
			t.Fatalf("backup command failed: %v %s", err, data)
		}
		var result map[string]any
		if json.Unmarshal(data, &result) != nil || result["passed"] != true {
			t.Fatal("invalid backup command result")
		}
		return result
	}
	snapshot := run("backup")
	filename := snapshot["filename"].(string)
	ciphertext, err := os.ReadFile(filepath.Join(root, "vault", filename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "private-share-fixture") || strings.Contains(string(ciphertext), "immutable-share-v1") {
		t.Fatal("plaintext backup")
	}
	restored := run("drill", "--backup", filename)
	if restored["quarantined"] != true || restored["recovery_quarantined"] != true || restored["disposable_database_removed"] != true || restored["tables"] != float64(36) {
		t.Fatal("incomplete drill")
	}
	folders, _ := filepath.Glob(filepath.Join(root, ".operations", "albums-restore-*"))
	for _, path := range folders {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			t.Fatal("restore database retained", err)
		}
	}
	// The production-facing source was never quarantined or rewritten.
	if err := CheckRecoveryState(context.Background(), f.db); err != nil {
		t.Fatal("source recovery state changed", err)
	}
	var state string
	if err = f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, f.login.AccountID).Scan(&state); err != nil || state != "active" {
		t.Fatal("source was changed", err)
	}
	if _, err = f.repo.AssetVersion(context.Background(), f.login.AccountID, f.version.VersionID); err != nil {
		t.Fatal("source media missing", err)
	}
	oldLedger := run("ledger")
	if _, err = accounts.requestAccountDeletion(context.Background(), f.session.Token, "10000000-0000-4000-8000-000000000098", sessions.now); err != nil {
		t.Fatal(err)
	}
	currentLedger := run("ledger")
	ledgerPath := filepath.Join(root, "vault", currentLedger["filename"].(string))
	reconciled := run("drill", "--backup", filename, "--deletion-ledger", ledgerPath, "--deletion-ledger-sha256", currentLedger["sha256"].(string))
	if reconciled["deletion_ledger_applied"] != true || reconciled["deletion_intents"] != float64(2) {
		t.Fatal("post-snapshot deletion not restored")
	}
	// Controlled object transport, real OpenSSL bundle encryption and native
	// MySQL restore. This proves the pipeline, not a live COS publication.
	offsiteRoundtrip := `import sys,json,shutil
from pathlib import Path
sys.path.insert(0,sys.argv[1])
from backup import Runtime,configure,drill
from ops_backup import file_digest
import offsite
r=Runtime(sys.argv[2],sys.argv[3],sys.argv[4]);configure(r)
remote=Path(sys.argv[2])/"controlled-offsite.enc"
def transfer(runtime,executable,credentials,operation,path,key,digest,size,**pinned):
    object_path=remote.with_name(Path(key).name)
    if operation=="put": shutil.copyfile(path,object_path)
    else: shutil.copyfile(object_path,path)
    assert file_digest(path)==digest and Path(path).stat().st_size==size
    return dict(Key=key,SHA256=digest,Bytes=size,Version="fixture-version",Bucket="fixture-1250000000",Region="ap-guangzhou")
offsite.move=transfer
with r.lock():
    receipt=offsite.publish(r,"controlled","controlled",sys.argv[5],sys.argv[6],sys.argv[7])
    recovered=offsite.fetch(r,"controlled","controlled",receipt["receipt"],receipt["receipt_sha256"])
    restored=Runtime(sys.argv[2],recovered["directory"],sys.argv[4]);configure(restored)
    result=drill(restored,Path(sys.argv[8]),recovered["snapshot"],recovered["deletion_ledger"],recovered["deletion_ledger_sha256"])
    assert result["recovery_quarantined"] and result["deletion_intents"]==2 and result["tables"]==36
print(json.dumps(dict(passed=True,quarantined=True,live_cos=False)))
`
	offsiteOutput, offsiteErr := exec.Command("python3", "-c", offsiteRoundtrip, filepath.Dir(script), root, filepath.Join(root, "vault"), envPath, filename, ledgerPath, currentLedger["sha256"].(string), binaries).CombinedOutput()
	if offsiteErr != nil {
		t.Fatalf("controlled encrypted offsite restore failed: %v %s", offsiteErr, offsiteOutput)
	}
	var offsiteResult map[string]any
	if json.Unmarshal(offsiteOutput, &offsiteResult) != nil || offsiteResult["passed"] != true {
		t.Fatal("invalid offsite restore result")
	}
	newSnapshot := run("backup")
	oldLedgerPath := filepath.Join(root, "vault", oldLedger["filename"].(string))
	// Re-sign the older synthetic checkpoint with a current clock to exercise
	// missing-intent detection in SQL, independently of the age check.
	restamp := `import sys,json,time
sys.path.insert(0,sys.argv[1])
from backup import Runtime,configure
from ops_backup import backup_secret,manifest_signature,file_digest
from ops_common import atomic_json
r=Runtime(sys.argv[2],sys.argv[3],sys.argv[4]);configure(r)
e=json.load(open(sys.argv[5]));e["payload"]["exported_at"]=int(time.time());e["hmac_sha256"]=manifest_signature(backup_secret(r),e["payload"])
atomic_json(sys.argv[5],e);print(file_digest(sys.argv[5]))
`
	restamped, restampErr := exec.Command("python3", "-c", restamp, filepath.Dir(script), root, filepath.Join(root, "vault"), envPath, oldLedgerPath).Output()
	if restampErr != nil {
		t.Fatal("synthetic checkpoint restamp", restampErr)
	}
	oldLedger["sha256"] = strings.TrimSpace(string(restamped))
	badArgs := []string{script, "drill", "--backend-dir", root, "--environment-file", envPath, "--backup-dir", filepath.Join(root, "vault"), "--mysql-bin-dir", binaries, "--backup", newSnapshot["filename"].(string), "--deletion-ledger", oldLedgerPath, "--deletion-ledger-sha256", oldLedger["sha256"].(string)}
	if exec.Command("python3", badArgs...).Run() == nil {
		t.Fatal("older ledger removed a deletion present in snapshot")
	}
	if _, err = f.db.Exec(`DELETE FROM album_deletion_ledger WHERE account_id=?`, f.login.AccountID); err != nil {
		t.Fatal(err)
	}
	badExport := []string{script, "ledger", "--backend-dir", root, "--environment-file", envPath, "--backup-dir", filepath.Join(root, "vault"), "--mysql-bin-dir", binaries}
	if exec.Command("python3", badExport...).Run() == nil {
		t.Fatal("export hid a missing deletion intent")
	}
	if _, err = f.db.Exec(`CREATE TABLE album_future_private (id INT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	args := []string{script, "backup", "--backend-dir", root, "--environment-file", envPath, "--backup-dir", filepath.Join(root, "vault"), "--mysql-bin-dir", binaries}
	if err = exec.Command("python3", args...).Run(); err == nil {
		t.Fatal("unreviewed schema silently backed up")
	}
}
