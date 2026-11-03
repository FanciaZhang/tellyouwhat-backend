package migrations

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMySQLAlbumDevelopmentProductsPreserveRowsAndRecoverUnrecordedDDL(t *testing.T) {
	const migration = "0073_album_development_subscription_products.sql"
	for _, variant := range []struct {
		name                string
		interrupted, scoped bool
	}{
		{"legacy", false, false}, {"DDL-before-receipt", true, false},
		{"scoped-legacy", false, true}, {"scoped-DDL-before-receipt", true, true},
	} {
		interrupted := variant.interrupted
		t.Run(variant.name, func(t *testing.T) {
			db := migrationTestDatabase(t)
			ctx := context.Background()
			if _, err := db.Exec(`CREATE TABLE schema_migrations(name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))`); err != nil {
				t.Fatal(err)
			}
			entries, err := fs.ReadDir(migrationFiles, ".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !isMigrationFile(entry.Name()) || entry.Name() >= migration || (variant.scoped && entry.Name() > "0064_album_identity_retirement.sql") {
					continue
				}
				sql, err := migrationFiles.ReadFile(entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.ExecContext(ctx, string(sql)); err != nil {
					t.Fatal(entry.Name(), err)
				}
				if _, err = db.Exec(`INSERT INTO schema_migrations(name) VALUES(?)`, entry.Name()); err != nil {
					t.Fatal(err)
				}
			}
			owner := uuid.NewString()
			if _, err := db.Exec(`INSERT INTO album_storage_accounts(owner_id,quota_bytes,used_bytes,reserved_bytes) VALUES(?,50000000000,12,3)`, owner); err != nil {
				t.Fatal(err)
			}
			insert := func(environment, product, key string, quota int64) error {
				now := time.Now().UTC()
				_, err := db.Exec(`INSERT INTO album_cloud_subscriptions(environment,original_transaction_id,owner_id,transaction_id,product_id,quota_bytes,active,expires_at,signed_at,verified_at) VALUES(?,?,?,?,?,?,true,?,?,?)`, environment, key, owner, key, product, quota, now.Add(time.Hour), now, now)
				return err
			}
			if err := insert("Production", "albums.cloud.50gb.annual", "old-production", 50_000_000_000); err != nil {
				t.Fatal(err)
			}
			if err := insert("Sandbox", "albums.cloud.500gb.annual", "old-sandbox", 500_000_000_000); err != nil {
				t.Fatal(err)
			}
			if err := insert("Sandbox", "albums.cloud.50gb.annual.dev", "rejected-before-upgrade", 50_000_000_000); err == nil {
				t.Fatal("old constraint unexpectedly accepted development SKU")
			}
			if interrupted {
				sql, err := migrationFiles.ReadFile(migration)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(string(sql)); err != nil {
					t.Fatal(err)
				}
				if err = insert("Sandbox", "albums.cloud.50gb.annual.dev", "before-receipt", 50_000_000_000); err != nil {
					t.Fatal(err)
				}
			}
			run := Run
			if variant.scoped {
				run = RunAlbumDevelopmentProducts
			}
			for range 2 {
				if err := run(ctx, db); err != nil {
					t.Fatal(err)
				}
			}
			for _, product := range []struct {
				id    string
				quota int64
			}{{"albums.cloud.50gb.annual.dev", 50_000_000_000}, {"albums.cloud.500gb.annual.dev", 500_000_000_000}} {
				if err := insert("Sandbox", product.id, product.id, product.quota); err != nil {
					t.Fatal(err)
				}
				if err := insert("Production", product.id, "foreign-"+product.id, product.quota); err == nil {
					t.Fatal("development SKU entered Production")
				}
				if err := insert("Sandbox", product.id, "wrong-quota-"+product.id, 1); err == nil {
					t.Fatal("wrong capacity accepted")
				}
			}
			var used, reserved, quota int64
			var count int
			var product string
			if err := db.QueryRow(`SELECT used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&used, &reserved); err != nil || used != 12 || reserved != 3 {
				t.Fatal("storage changed", used, reserved, err)
			}
			if err := db.QueryRow(`SELECT product_id,quota_bytes FROM album_cloud_subscriptions WHERE original_transaction_id='old-production'`).Scan(&product, &quota); err != nil || product != "albums.cloud.50gb.annual" || quota != 50_000_000_000 {
				t.Fatal("legacy purchase changed", product, quota, err)
			}
			if variant.scoped {
				if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name > '0064_album_identity_retirement.sql' AND name NOT IN (?, '0074_album_system_copy_locators.sql')`, migration).Scan(&count); err != nil || count != 0 {
					t.Fatal("scoped job applied unrelated migrations", count, err)
				}
			}
			if _, err := db.Exec(`SELECT owner_id,version_id,locator_id,copy_kind,cloud_identifier FROM album_system_copy_locators LIMIT 0`); err != nil {
				t.Fatal("copy locator migration missing", err)
			}
			version := uuid.NewString()
			if _, err := db.Exec(`INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at) VALUES(?,'synthetic',?,REPEAT('a',64),'{}','[]',1,NOW(6))`, owner, version); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO album_system_copy_locators(owner_id,version_id,locator_id,copy_kind,cloud_identifier) VALUES(?,?,REPEAT('b',64),'restored_original','synthetic-copy')`, owner, version); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name='0074_album_system_copy_locators.sql'`); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := run(ctx, db); err != nil {
					t.Fatal("copy DDL receipt replay", err)
				}
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM album_system_copy_locators WHERE owner_id=? AND version_id=? AND cloud_identifier='synthetic-copy'`, owner, version).Scan(&count); err != nil || count != 1 {
				t.Fatal("copy hint lost in migration replay", count, err)
			}
			expected := 4
			if interrupted {
				expected++
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM album_cloud_subscriptions`).Scan(&count); err != nil || count != expected {
				t.Fatal("lost or invented purchase", count, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, migration).Scan(&count); err != nil || count != 1 {
				t.Fatal("migration receipt", count, err)
			}
		})
	}
}

func TestMySQLAlbumScopedMigrationRequiresExistingBase(t *testing.T) {
	db := migrationTestDatabase(t)
	if err := RunAlbumDevelopmentProducts(context.Background(), db); err == nil {
		t.Fatal("empty database accepted")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()`).Scan(&count); err != nil || count != 0 {
		t.Fatal("empty database mutated", count, err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(name VARCHAR(255) PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := RunAlbumDevelopmentProducts(context.Background(), db); err == nil {
		t.Fatal("unrelated schema accepted")
	}
}
