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
	other, err := accounts.RegisterAppleLogin(context.Background(), AppleGrant{Identity: AppleIdentity{Subject: "recovery-deleting-fixture"}, RefreshToken: "synthetic-refresh"})
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
	if restored["quarantined"] != true || restored["disposable_database_removed"] != true || restored["tables"] != float64(34) {
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
	var state string
	if err = f.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, f.login.AccountID).Scan(&state); err != nil || state != "active" {
		t.Fatal("source was changed", err)
	}
	if _, err = f.repo.AssetVersion(context.Background(), f.login.AccountID, f.version.VersionID); err != nil {
		t.Fatal("source media missing", err)
	}
	if _, err = f.db.Exec(`CREATE TABLE album_future_private (id INT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	args := []string{script, "backup", "--backend-dir", root, "--environment-file", envPath, "--backup-dir", filepath.Join(root, "vault"), "--mysql-bin-dir", binaries}
	if err = exec.Command("python3", args...).Run(); err == nil {
		t.Fatal("unreviewed schema silently backed up")
	}
}
