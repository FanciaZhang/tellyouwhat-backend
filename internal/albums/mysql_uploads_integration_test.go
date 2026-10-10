package albums

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/migrations"
)

func TestMySQLAlbumReservationAndVerificationTransactions(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MYSQL_TEST_DSN"))
	if dsn == "" {
		t.Skip("set MYSQL_TEST_DSN for real MySQL album transaction tests")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(config.DBName, "_test") {
		t.Fatal("refusing non-test MySQL database")
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
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	r := NewMySQLUploads(db)
	_, _, objects, owner, manifest := uploadFixture(t)
	defer func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM album_uploads WHERE owner_id=?`, owner)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM album_storage_accounts WHERE owner_id=?`, owner)
	}()
	bytes := manifest.Resources[0].SizeBytes
	if err := r.ProvisionAccount(ctx, owner, bytes*6); err != nil {
		t.Fatal(err)
	}
	// Replayed provisioning must not replace a paid account's limit.
	if err := r.ProvisionAccount(ctx, owner, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	s, err := NewUploadService(r, objects, UploadPolicy{1 << 30, 1 << 28, time.Hour, time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	won := make(chan Upload, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			m := manifest
			m.AssetID = fmt.Sprintf("asset-%d", i)
			u, err := s.Create(ctx, owner, uuid.NewString(), m)
			if err == nil {
				won <- u
			} else {
				failures <- err
			}
		}(i)
	}
	workers.Wait()
	close(won)
	close(failures)
	var uploads []Upload
	for u := range won {
		uploads = append(uploads, u)
	}
	if len(uploads) != 3 {
		t.Fatalf("concurrent quota allowed %d uploads, want 3", len(uploads))
	}
	for err := range failures {
		if !errors.Is(err, ErrQuota) {
			t.Fatalf("unexpected concurrent failure: %v", err)
		}
	}
	u := uploads[0]
	replay, err := s.Create(ctx, owner, u.RequestID, u.Manifest)
	if err != nil || replay.ID != u.ID {
		t.Fatalf("idempotency: %+v %v", replay, err)
	}
	sameVersion, err := s.Create(ctx, owner, uuid.NewString(), u.Manifest)
	if err != nil || sameVersion.ID != u.ID {
		t.Fatalf("same version charged twice: %v", err)
	}
	changed := u.Manifest
	changed.SourceRevision = "new-version"
	if _, err := s.Create(ctx, owner, u.RequestID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("manifest change: %v", err)
	}
	if _, err := r.Get(ctx, uuid.NewString(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross account: %v", err)
	}
	if _, err := r.Submit(ctx, owner, u.ID, now); err != nil {
		t.Fatal(err)
	}
	first, second := uuid.NewString(), uuid.NewString()
	if _, err := r.Claim(ctx, owner, u.ID, first, now, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(ctx, owner, u.ID, second, now, now.Add(time.Minute)); !errors.Is(err, ErrLease) {
		t.Fatalf("live lease stolen: %v", err)
	}
	if _, err := r.Claim(ctx, owner, u.ID, second, now.Add(2*time.Second), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	resource := u.Manifest.Resources[0]
	proof := []SealedObject{{ResourceID: resource.ID, Key: sealedKey(u, resource.ID), VersionID: "immutable-v1", SizeBytes: resource.SizeBytes, SHA256: resource.SHA256}}
	if _, err := r.Complete(ctx, owner, u.ID, first, proof, now.Add(3*time.Second)); !errors.Is(err, ErrLease) {
		t.Fatalf("stale worker committed: %v", err)
	}
	if _, err := r.Complete(ctx, owner, u.ID, second, proof, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Complete(ctx, owner, u.ID, second, proof, now.Add(4*time.Second)); err != nil {
		t.Fatalf("completion replay: %v", err)
	}
	var used, reserved int64
	if err := db.QueryRowContext(ctx, `SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != bytes || reserved != 5*bytes {
		t.Fatalf("ledger used=%d reserved=%d, want %d %d", used, reserved, bytes, 5*bytes)
	}
	stored, err := r.Get(ctx, owner, u.ID)
	if err != nil || stored.State != OriginalsVerified || len(stored.Objects) != 1 {
		t.Fatalf("durable proof: %+v %v", stored, err)
	}
}
