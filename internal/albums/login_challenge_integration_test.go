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
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLLoginChallengeConsumedOnceAcrossServiceInstances(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MYSQL_TEST_DSN"))
	if dsn == "" {
		t.Skip("set MYSQL_TEST_DSN for durable challenge concurrency test")
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
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	service, _ := NewLoginChallengeService(NewMySQLLoginChallenges(db), func() time.Time { return now })
	challenge, err := service.Issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DELETE FROM album_login_challenges WHERE challenge_id=?`, challenge.ID)
	var storedHash string
	if err := db.QueryRowContext(ctx, `SELECT proof_sha256 FROM album_login_challenges WHERE challenge_id=?`, challenge.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == challenge.Proof || len(storedHash) != 64 {
		t.Fatal("invalid stored proof")
	}
	if _, err := service.Consume(ctx, challenge.ID, strings.Repeat("A", 43)); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("wrong proof accepted")
	}
	var won atomic.Int64
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			// Fresh service/repository for every contender, not a shared memory mutex.
			contender, _ := NewLoginChallengeService(NewMySQLLoginChallenges(db), func() time.Time { return now })
			nonce, err := contender.Consume(ctx, challenge.ID, challenge.Proof)
			if err == nil {
				won.Add(1)
				if nonce != challenge.Nonce {
					t.Error("nonce binding changed")
				}
			} else if !errors.Is(err, ErrLoginChallenge) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	group.Wait()
	if won.Load() != 1 {
		t.Fatalf("challenge consumed %d times", won.Load())
	}
	fresh, _ := NewLoginChallengeService(NewMySQLLoginChallenges(db), func() time.Time { return now })
	if _, err := fresh.Consume(ctx, challenge.ID, challenge.Proof); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("restart forgot consumption")
	}
	expired, err := service.Issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DELETE FROM album_login_challenges WHERE challenge_id=?`, expired.ID)
	atExpiry, _ := NewLoginChallengeService(NewMySQLLoginChallenges(db), func() time.Time { return expired.ExpiresAt })
	if _, err := atExpiry.Consume(ctx, expired.ID, expired.Proof); !errors.Is(err, ErrLoginChallenge) {
		t.Fatal("expired SQL challenge consumed")
	}
}
