package albums

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestMySQLRecoveryLedgerInvalidatesSubscriptionCompletionAtomically(t *testing.T) {
	binaries := os.Getenv("ALBUM_BACKUP_TEST_MYSQL_BIN")
	if binaries == "" {
		t.Skip("set ALBUM_BACKUP_TEST_MYSQL_BIN for native recovery SQL verification")
	}
	f, repository, service, _, deletion := completionFixture(t)
	if batch, err := service.runSubscriptionBindingBatch(context.Background(), repository, 10); err != nil || batch.Acknowledged != 1 {
		t.Fatal(batch, err)
	}
	before, checked, revision := readCompletionReceipt(t, f, deletion.AccountID)
	if !before.Valid || !checked.Valid {
		t.Fatal("fixture needs a previously verified complete subscription revision")
	}
	other, _ := f.login(t, "recovery-unrelated", f.now)
	var owner, request, digest, stamp string
	if err := f.r.db.QueryRow(`SELECT account_id,request_id,LOWER(HEX(authorizing_session_sha256)),DATE_FORMAT(requested_at,'%Y-%m-%dT%H:%i:%s.%fZ') FROM album_deletion_ledger WHERE account_id=?`, deletion.AccountID).Scan(&owner, &request, &digest, &stamp); err != nil {
		t.Fatal(err)
	}
	entries, err := json.Marshal([][]string{{owner, request, digest, stamp}})
	if err != nil {
		t.Fatal(err)
	}
	// Execute the actual recovery producer's SQL against this disposable schema,
	// using the same mysql batch failure/connection-close behavior as a drill.
	script := `import json,sys
sys.path.insert(0,sys.argv[1])
import backup
from deletion_ledger import restore_fences
class Capture:
    def execute(self,name,sql,*,input): sys.stdout.buffer.write(input)
restore_fences(Capture(),[],json.load(sys.stdin))
`
	path, err := filepath.Abs("../../deploy/albums")
	if err != nil {
		t.Fatal(err)
	}
	producer := exec.Command("python3", "-c", script, path)
	producer.Stdin = bytes.NewReader(entries)
	sql, err := producer.Output()
	if err != nil {
		t.Fatal("recovery SQL generation failed", err)
	}
	config, err := mysql.ParseDSN(f.dsn)
	if err != nil || config.Net != "unix" {
		t.Fatal("recovery integration requires isolated unix-socket MySQL", err)
	}
	apply := func() error {
		command := exec.Command(filepath.Join(binaries, "mysql"), "--no-defaults", "--protocol=socket", "--socket="+config.Addr, "--user="+config.User, "--database="+config.DBName, "--batch")
		command.Env = append(os.Environ(), "MYSQL_PWD="+config.Passwd)
		command.Stdin = bytes.NewReader(sql)
		return command.Run()
	}
	countAcknowledged := func() int {
		t.Helper()
		var count int
		if err := f.r.db.QueryRow(`SELECT COUNT(*) FROM album_deletion_subscription_jobs WHERE account_id=? AND acknowledged_at IS NOT NULL`, owner).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if countAcknowledged() != 2 {
		t.Fatal("fixture needs both individual acknowledgements")
	}
	// A failure after receipt invalidation must restore both kinds of evidence.
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs ADD CONSTRAINT fail_recovery_binding_reset CHECK(acknowledged_at IS NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := apply(); err == nil {
		t.Fatal("recovery accepted a failed job reset")
	}
	after, at, current := readCompletionReceipt(t, f, owner)
	if after != before || !at.Time.Equal(checked.Time) || current != revision || countAcknowledged() != 2 {
		t.Fatal("failed recovery partially invalidated subscription evidence")
	}
	if _, err := f.r.db.Exec(`ALTER TABLE album_deletion_subscription_jobs DROP CHECK fail_recovery_binding_reset`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := apply(); err != nil {
			t.Fatal("recovery replay failed", err)
		}
		after, at, current = readCompletionReceipt(t, f, owner)
		if after.Valid || at.Valid || current != revision || countAcknowledged() != 0 {
			t.Fatal("restored binding jobs retained stale complete verification")
		}
		var state string
		if err := f.r.db.QueryRow(`SELECT state FROM album_accounts WHERE account_id=?`, other.AccountID).Scan(&state); err != nil || state != "active" {
			t.Fatal("ledger reconciliation changed an unrelated account", err)
		}
	}
	// Fresh provider verification can establish a new receipt after recovery.
	if batch, err := service.runSubscriptionBindingBatch(context.Background(), repository, 10); err != nil || batch.Acknowledged != 2 {
		t.Fatal("fresh verification could not resume", batch, err)
	}
	after, at, current = readCompletionReceipt(t, f, owner)
	if !after.Valid || !at.Valid || after.Int64 != current || countAcknowledged() != 2 {
		t.Fatal("fresh complete verification not persisted")
	}
}
