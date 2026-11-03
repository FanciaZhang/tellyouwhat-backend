package mysqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestBirthdayMigrationAllowsEnrollmentAndPreservesAppSettings(t *testing.T) {
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var bundle string
	if err := db.QueryRowContext(ctx, `SELECT bundle_id FROM apps WHERE app_id = 'birthday'`).Scan(&bundle); err != nil || bundle != "cn.tellyouwhat.birthday" {
		t.Fatalf("Birthday app was not seeded: bundle=%s err=%v", bundle, err)
	}
	if err := mysqlstore.NewKeyRepository(db, "birthday").Register(ctx, attestation.RegisteredKey{
		KeyID: "birthday-enrollment", DeviceID: "00000000-0000-4000-8000-000000000083",
		PublicKey: []byte("fixture"), Environment: "development", Receipt: []byte("fixture"),
	}); err != nil {
		t.Fatalf("Birthday enrollment failed after migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apps SET display_name = CONCAT(app_id, '-configured'), enabled = FALSE`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = '0015_birthday_app.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(ctx, db); err != nil {
			t.Fatalf("Birthday migration replay failed: %v", err)
		}
	}
	var preserved int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE display_name = CONCAT(app_id, '-configured') AND enabled = FALSE`).Scan(&preserved); err != nil || preserved != 3 {
		t.Fatalf("Migration changed existing app settings: count=%d err=%v", preserved, err)
	}
}
