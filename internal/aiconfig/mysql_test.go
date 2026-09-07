package aiconfig_test

import (
	"context"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMySQLPublishCompareAndSwap(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatal("requires a dedicated _test database")
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := aiconfig.MySQLStore{DB: db}
	op := contracts.OperationMealDecision
	current, err := store.Current(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	base := ""
	if current != nil {
		base = current.ID
	}
	first, second := uuid.NewString(), uuid.NewString()
	defer func() {
		if base == "" {
			db.ExecContext(ctx, "DELETE FROM health_ai_config_current WHERE operation=? AND revision_id IN (?,?)", op, first, second)
		} else {
			db.ExecContext(ctx, "UPDATE health_ai_config_current SET revision_id=? WHERE operation=? AND revision_id IN (?,?)", base, op, first, second)
		}
		db.ExecContext(ctx, "DELETE FROM health_ai_config_revisions WHERE id IN (?,?)", first, second)
	}()
	for _, id := range []string{first, second} {
		r := aiconfig.Revision{ID: id, Operation: op, BaseVersion: base, CreatedBy: "integration-test", CreatedAt: time.Now(), Policy: contracts.ExecutionPolicy{Version: id, Endpoint: "ep-test", ReasoningEffort: "low", TimeoutSeconds: 90}}
		if err = store.Draft(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Publish(ctx, first, op, base, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.Publish(ctx, first, op, base, time.Now()); err != nil {
		t.Fatal("idempotent publish", err)
	}
	if err = store.Publish(ctx, second, op, base, time.Now()); err != aiconfig.ErrConflict {
		t.Fatalf("stale draft overwrote publication: %v", err)
	}
	published, err := store.Current(ctx, op)
	if err != nil || published.ID != first || published.PublishedAt == nil {
		t.Fatal("publication not persisted")
	}
}
