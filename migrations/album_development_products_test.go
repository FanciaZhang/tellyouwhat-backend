package migrations

import (
	"context"
	"io/fs"
	"strings"
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
				run = RunAlbumDevelopmentMetadata
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
				if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name > '0064_album_identity_retirement.sql' AND name NOT IN (?, '0074_album_system_copy_locators.sql','0075_album_content_corrections.sql','0076_album_asset_favorites.sql','0077_album_collections.sql','0078_album_asset_deletion_ledger.sql','0080_album_photo_multipart.sql','0081_album_photo_completion_fence.sql','0082_album_retention_cleanup_schedule.sql','0083_album_record_erasure_schedule.sql')`, migration).Scan(&count); err != nil || count != 0 {
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
	if err := RunAlbumDevelopmentMetadata(context.Background(), db); err == nil {
		t.Fatal("empty database accepted")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()`).Scan(&count); err != nil || count != 0 {
		t.Fatal("empty database mutated", count, err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(name VARCHAR(255) PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := RunAlbumDevelopmentMetadata(context.Background(), db); err == nil {
		t.Fatal("unrelated schema accepted")
	}
}

func TestMySQLAlbumDevelopmentMetadataPreservesMediaAndReplaysDDL(t *testing.T) {
	metadata := []string{"0075_album_content_corrections.sql", "0076_album_asset_favorites.sql",
		"0077_album_collections.sql", "0078_album_asset_deletion_ledger.sql", "0080_album_photo_multipart.sql", "0081_album_photo_completion_fence.sql", "0082_album_retention_cleanup_schedule.sql", "0083_album_record_erasure_schedule.sql"}
	for _, interrupted := range append([]string{""}, metadata...) {
		name := interrupted
		if name == "" {
			name = "from-deployed-0074"
		}
		t.Run(name, func(t *testing.T) {
			db := migrationTestDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := db.Exec(`CREATE TABLE schema_migrations(name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))`); err != nil {
				t.Fatal(err)
			}
			entries, err := fs.ReadDir(migrationFiles, ".")
			if err != nil {
				t.Fatal(err)
			}
			apply := func(name string, record bool) {
				t.Helper()
				body, err := migrationFiles.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, string(body)); err != nil {
					t.Fatal(name, err)
				}
				if record {
					if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(name) VALUES(?)`, name); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, entry := range entries {
				if isMigrationFile(entry.Name()) && (entry.Name() <= "0064_album_identity_retirement.sql" ||
					entry.Name() == "0073_album_development_subscription_products.sql" || entry.Name() == "0074_album_system_copy_locators.sql") {
					apply(entry.Name(), true)
				}
			}
			owner, version, deleted := uuid.NewString(), uuid.NewString(), uuid.NewString()
			seed := []struct {
				query string
				args  []any
			}{
				{`INSERT INTO album_accounts(account_id,apple_subject_sha256) VALUES(?,REPEAT('a',64))`, []any{owner}},
				{`INSERT INTO album_account_deletions(account_id,request_id,authorizing_session_sha256,requested_at) VALUES(?,?,UNHEX(REPEAT('a',64)),'2026-10-01')`, []any{owner, uuid.NewString()}},
				{`INSERT INTO album_storage_accounts(owner_id,quota_bytes,used_bytes,reserved_bytes) VALUES(?,50000000000,12,3)`, []any{owner}},
				{`INSERT INTO album_asset_lifecycle(owner_id,asset_id,revision,lifecycle_epoch,state) VALUES(?,'active-media',1,1,'active'),(?,'deleted-media',3,3,'purged')`, []any{owner, owner}},
				{`INSERT INTO album_asset_versions(owner_id,asset_id,version_id,manifest_digest,manifest_json,sealed_objects,original_bytes,published_at) VALUES(?,'active-media',?,REPEAT('b',64),'{}','[]',12,'2026-10-01 00:00:00')`, []any{owner, version}},
				{`INSERT INTO album_system_copy_locators(owner_id,version_id,locator_id,copy_kind,cloud_identifier) VALUES(?,?,REPEAT('c',64),'restored_original','synthetic-locator')`, []any{owner, version}},
				{`INSERT INTO album_library_changes(owner_id,asset_id,version_id,revision,state,changed_at) VALUES(?,'active-media',?,1,'active','2026-10-01 00:00:00'),(?,'deleted-media',?,3,'purged','2026-10-01 00:00:00')`, []any{owner, version, owner, deleted}},
				{`INSERT INTO album_object_purges(owner_id,asset_id,revision,trash_expires_at,targets_json,state,available_at,updated_at,refunded_bytes,completed_at) VALUES(?,'deleted-media',2,'2026-10-01 00:00:00','[]','completed','2026-10-01 00:00:00','2026-10-01 00:00:00',4,'2026-10-01 00:00:00')`, []any{owner}},
			}
			for _, item := range seed {
				if _, err := db.ExecContext(ctx, item.query, item.args...); err != nil {
					t.Fatal("synthetic baseline", err)
				}
			}
			for i, preset := range []string{"grid_jpeg_v1", "daily_jpeg_v1"} {
				var object any
				if i == 1 {
					object = "legacy-photo-version"
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,object_version,created_at,updated_at) VALUES(?,?,?,REPEAT('b',64),?,3,REPEAT('d',64),7,5,?,'2026-10-01','2026-10-01')`, uuid.NewString(), owner, version, preset, object); err != nil {
					t.Fatal("legacy photo write", err)
				}
			}
			if interrupted != "" {
				for _, migration := range metadata {
					if migration == "0081_album_photo_completion_fence.sql" {
						for i, preset := range []string{"grid_png_v1", "daily_png_v1"} {
							var object any
							if i == 1 {
								object = "old-multipart-version"
							}
							if _, err := db.ExecContext(ctx, `INSERT INTO album_photo_writes(attempt_id,owner_id,version_id,manifest_digest,preset,size_bytes,sha256,pixel_width,pixel_height,object_version,created_at,updated_at,multipart,provider_upload_id) VALUES(?,?,?,REPEAT('b',64),?,3,REPEAT('e',64),7,5,?,'2026-10-01','2026-10-01',TRUE,'old-multipart-session')`, uuid.NewString(), owner, version, preset, object); err != nil {
								t.Fatal("old multipart write", err)
							}
						}
					}
					apply(migration, migration != interrupted)
					if migration == interrupted {
						break
					}
				}
			}
			if interrupted == "0082_album_retention_cleanup_schedule.sql" {
				if _, err := db.Exec(`UPDATE album_storage_accounts SET retention_cleanup_after='2026-10-02',retention_cleanup_token=?,retention_cleanup_until='2026-10-03',retention_cleanup_cursor='active-media' WHERE owner_id=?`, owner, owner); err != nil {
					t.Fatal(err)
				}
			}
			if interrupted == "0083_album_record_erasure_schedule.sql" {
				if _, err := db.Exec(`UPDATE album_account_deletions SET record_erase_after='2026-10-02' WHERE account_id=?`, owner); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := RunAlbumDevelopmentMetadata(ctx, db); err != nil {
					t.Fatal("scoped metadata replay", err)
				}
			}
			var reason, receiptVersion, locator string
			var revision int64
			var count int
			recordQuery := `SELECT COUNT(*) FROM album_account_deletions WHERE account_id=? AND record_erase_after IS NULL`
			if interrupted == "0083_album_record_erasure_schedule.sql" {
				recordQuery = `SELECT COUNT(*) FROM album_account_deletions WHERE account_id=? AND record_erase_after='2026-10-02'`
			}
			if err := db.QueryRow(recordQuery, owner).Scan(&count); err != nil || count != 1 {
				t.Fatal("record erasure schedule default or interrupted DDL state changed", count, err)
			}
			query := `SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND retention_cleanup_after IS NULL AND retention_cleanup_token IS NULL AND retention_cleanup_until IS NULL AND retention_cleanup_cursor IS NULL`
			args := []any{owner}
			if interrupted == "0082_album_retention_cleanup_schedule.sql" {
				query = `SELECT COUNT(*) FROM album_storage_accounts WHERE owner_id=? AND retention_cleanup_after='2026-10-02' AND retention_cleanup_token=? AND retention_cleanup_until='2026-10-03' AND retention_cleanup_cursor='active-media'`
				args = append(args, owner)
			}
			if err := db.QueryRow(query, args...).Scan(&count); err != nil || count != 1 {
				t.Fatal("retention schedule default or interrupted DDL state changed", count, err)
			}
			if err := db.QueryRow(`SELECT final_revision,reason,JSON_UNQUOTE(JSON_EXTRACT(version_ids,'$[0]')) FROM album_asset_deletion_ledger WHERE owner_id=? AND asset_id='deleted-media' AND JSON_LENGTH(version_ids)=1`, owner).Scan(&revision, &reason, &receiptVersion); err != nil || revision != 3 || reason != "trash" || receiptVersion != deleted {
				t.Fatal("completed purge receipt missing or changed", revision, reason, receiptVersion, err)
			}
			var used, reserved, quota, bytes int64
			if err := db.QueryRow(`SELECT quota_bytes,used_bytes,reserved_bytes FROM album_storage_accounts WHERE owner_id=?`, owner).Scan(&quota, &used, &reserved); err != nil || quota != 50_000_000_000 || used != 12 || reserved != 3 {
				t.Fatal("historical balance changed", err)
			}
			if err := db.QueryRow(`SELECT original_bytes FROM album_asset_versions WHERE owner_id=? AND version_id=? AND manifest_digest=REPEAT('b',64) AND manifest_json=JSON_OBJECT() AND sealed_objects=JSON_ARRAY()`, owner, version).Scan(&bytes); err != nil || bytes != 12 {
				t.Fatal("original manifest changed", err)
			}
			if err := db.QueryRow(`SELECT cloud_identifier FROM album_system_copy_locators WHERE owner_id=? AND version_id=?`, owner, version).Scan(&locator); err != nil || locator != "synthetic-locator" {
				t.Fatal("system locator changed", err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM album_library_changes WHERE owner_id=?`, owner).Scan(&count); err != nil || count != 2 {
				t.Fatal("migration invented or erased library events", count, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM album_photo_writes WHERE owner_id=? AND multipart=FALSE AND provider_upload_id IS NULL AND multipart_phase='unknown' AND sha256=REPEAT('d',64) AND pixel_width=7 AND pixel_height=5 AND size_bytes=3 AND (object_version IS NULL OR object_version='legacy-photo-version')`, owner).Scan(&count); err != nil || count != 2 {
				t.Fatal("migration changed legacy photo writes", count, err)
			}
			if interrupted == "0081_album_photo_completion_fence.sql" {
				if err := db.QueryRow(`SELECT COUNT(*) FROM album_photo_writes WHERE owner_id=? AND multipart=TRUE AND provider_upload_id='old-multipart-session' AND multipart_phase='unknown' AND sha256=REPEAT('e',64) AND (object_version IS NULL OR object_version='old-multipart-version')`, owner).Scan(&count); err != nil || count != 2 {
					t.Fatal("migration invented unissued Complete evidence", count, err)
				}
			}
			for _, table := range []string{"album_content_corrections", "album_asset_favorites", "album_collections", "album_collection_members"} {
				if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("migration invented user choices", table, count, err)
				}
			}
			rows, err := db.Query(`SELECT name FROM schema_migrations WHERE name>'0064_album_identity_retirement.sql' ORDER BY name`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var names []string
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
			}
			if err := rows.Err(); err != nil || strings.Join(names, ",") != "0073_album_development_subscription_products.sql,0074_album_system_copy_locators.sql,"+strings.Join(metadata, ",") {
				t.Fatal("scoped job applied unrelated or incomplete migrations", names, err)
			}
		})
	}
}
