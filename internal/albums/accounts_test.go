package albums

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func TestAppleGrantEncryptionBindsAccountIdentityAndGrant(t *testing.T) {
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	account, grant := uuid.NewString(), uuid.NewString()
	hash := appleSubjectHash("subject")
	encrypted, nonce, err := cipher.Encrypt([]byte("private-refresh-token"), appleGrantAAD(account, hash, grant))
	if err != nil {
		t.Fatal(err)
	}
	for _, aad := range [][]byte{appleGrantAAD(uuid.NewString(), hash, grant), appleGrantAAD(account, appleSubjectHash("other"), grant), appleGrantAAD(account, hash, uuid.NewString())} {
		if _, err := cipher.Decrypt(encrypted, nonce, aad); err == nil {
			t.Fatal("credential could be transplanted")
		}
	}
	if _, err := NewMySQLAccounts(nil, cipher); err == nil {
		t.Fatal("missing database accepted")
	}
}

func TestMySQLAppleAccountStableAcrossConcurrentLogins(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MYSQL_TEST_DSN"))
	if dsn == "" {
		t.Skip("set MYSQL_TEST_DSN for stable album account tests")
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
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewMySQLAccounts(db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	subject := uuid.NewString()
	defer func() {
		var account string
		if db.QueryRowContext(context.Background(), `SELECT account_id FROM album_accounts WHERE apple_subject_sha256=?`, appleSubjectHash(subject)).Scan(&account) == nil {
			db.ExecContext(context.Background(), `DELETE FROM album_apple_grants WHERE account_id=?`, account)
			db.ExecContext(context.Background(), `DELETE FROM album_storage_accounts WHERE owner_id=?`, account)
			db.ExecContext(context.Background(), `DELETE FROM album_accounts WHERE account_id=?`, account)
		}
	}()
	verified := AppleGrant{Identity: AppleIdentity{Subject: subject}, RefreshToken: "private-refresh-token"}
	var group sync.WaitGroup
	logins := make(chan AccountLogin, 10)
	for range 10 {
		group.Add(1)
		go func() {
			defer group.Done()
			login, err := repo.RegisterAppleLogin(ctx, verified)
			if err != nil {
				t.Errorf("login transaction: %v", err)
				return
			}
			logins <- login
		}()
	}
	group.Wait()
	close(logins)
	var first AccountLogin
	var count int
	for login := range logins {
		count++
		if first.AccountID == "" {
			first = login
		}
		if login != first {
			t.Error("same Apple grant created multiple accounts/grants")
		}
	}
	if count != 10 {
		t.Fatalf("only %d logins succeeded", count)
	}
	var quota int64
	if err := db.QueryRowContext(ctx, `SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, first.AccountID).Scan(&quota); err != nil || quota != 0 {
		t.Fatalf("unexpected free quota %d %v", quota, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_storage_accounts SET quota_bytes=12345 WHERE owner_id=?`, first.AccountID); err != nil {
		t.Fatal(err)
	}
	verified.RefreshToken = "another-device-refresh"
	second, err := repo.RegisterAppleLogin(ctx, verified)
	if err != nil || second.AccountID != first.AccountID || second.GrantID == first.GrantID {
		t.Fatal("second device lost stable account or original grant", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT quota_bytes FROM album_storage_accounts WHERE owner_id=?`, first.AccountID).Scan(&quota); err != nil || quota != 12345 {
		t.Fatal("login replaced quota", err)
	}
	// A new service instance must recover the same identity without memory state.
	restarted, err := NewMySQLAccounts(db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if login, err := restarted.RegisterAppleLogin(ctx, verified); err != nil || login != second {
		t.Fatal("restart changed account or grant", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_apple_grants SET revoked_at=UTC_TIMESTAMP(6) WHERE grant_id=?`, second.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RegisterAppleLogin(ctx, verified); !errors.Is(err, ErrAppleCode) {
		t.Fatal("revoked grant accepted", err)
	}
	// Revocation work still needs the credential after the grant is disabled.
	if token, err := restarted.RefreshCredential(ctx, second.AccountID, second.GrantID); err != nil || token != verified.RefreshToken {
		t.Fatal("revoked credential unavailable for provider cleanup", err)
	}
	token, err := repo.RefreshCredential(ctx, first.AccountID, first.GrantID)
	if err != nil || token != "private-refresh-token" {
		t.Fatal("old device credential lost", err)
	}
	if _, err := repo.RefreshCredential(ctx, uuid.NewString(), first.GrantID); !errors.Is(err, ErrOwner) {
		t.Fatal("cross-account grant readable")
	}
	var encrypted []byte
	if err := db.QueryRowContext(ctx, `SELECT encrypted_token FROM album_apple_grants WHERE grant_id=?`, first.GrantID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("private-refresh-token")) {
		t.Fatal("refresh token stored in plaintext")
	}
	encrypted[0] ^= 1
	if _, err := db.ExecContext(ctx, `UPDATE album_apple_grants SET encrypted_token=? WHERE grant_id=?`, encrypted, first.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RefreshCredential(ctx, first.AccountID, first.GrantID); !errors.Is(err, ErrAppleCode) {
		t.Fatal("tampered credential decrypted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE album_accounts SET state='disabled' WHERE account_id=?`, first.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RegisterAppleLogin(ctx, verified); !errors.Is(err, ErrOwner) {
		t.Fatal("disabled account reactivated")
	}
}
