package migrations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func TestMySQLAlbumAppAttestRegistryAndReplayPreserveOtherApps(t *testing.T) {
	db := migrationTestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := RunIsolatedTest(ctx, db); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE app_id='albums' AND managed_product_id=''`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("album authentication registry is absent or received AI commerce", err)
	}
	for _, app := range []string{"health", "albums"} {
		keys := mysqlstore.NewKeyRepository(db, app)
		key := attestation.RegisteredKey{AppID: app, KeyID: "synthetic-migration-key", DeviceID: "44444444-4444-4444-8444-444444444444",
			PublicKey: []byte("synthetic-public-key"), Environment: "production", Receipt: []byte("synthetic-receipt")}
		if err := keys.Register(ctx, key); err != nil {
			t.Fatal("scoped key foreign key rejected", err)
		}
		if err := keys.AdvanceCounter(ctx, key.KeyID, 0, 1); err != nil {
			t.Fatal(err)
		}
		if err := keys.AdvanceCounter(ctx, key.KeyID, 0, 2); !errors.Is(err, attestation.ErrReplay) {
			t.Fatal("counter replay accepted", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE apps SET display_name=?,enabled=FALSE WHERE app_id=?`, "preserved-"+app, app); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := migrationFiles.ReadFile("0079_platform_albums_app_attest.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(contents)); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"health", "albums"} {
		var name string
		var enabled bool
		if err := db.QueryRowContext(ctx, `SELECT display_name,enabled FROM apps WHERE app_id=?`, app).Scan(&name, &enabled); err != nil || name != "preserved-"+app || enabled {
			t.Fatal("migration overwrote an existing registry", err)
		}
		key, err := mysqlstore.NewKeyRepository(db, app).Get(ctx, "synthetic-migration-key")
		if err != nil || key.AppID != app || key.Counter != 1 {
			t.Fatal("migration lost or crossed an existing key", err)
		}
	}
}
