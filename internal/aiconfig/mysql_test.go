package aiconfig_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/migrations"
)

func mysqlFixture(t *testing.T) (aiconfig.MySQLStore, string) {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN is required")
	}
	ctx := context.Background()
	db, err := mysqlstore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var name string
	if err = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "_test") {
		t.Fatal("requires an isolated _test database")
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	actor := uuid.NewString()
	_, err = db.ExecContext(ctx, `INSERT INTO admin_users(id,webauthn_id,display_name,role) VALUES(?,?,?,'admin')`, actor, []byte(actor), "AI integration fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE c FROM health_ai_config_current c JOIN health_ai_config_revisions r ON r.id=c.revision_id WHERE r.document->>'$.createdBy'=?`, actor)
		db.Exec(`DELETE FROM health_ai_config_revisions WHERE document->>'$.createdBy'=?`, actor)
		db.Exec(`DELETE FROM admin_audit_events WHERE admin_user_id=?`, actor)
		db.Exec(`DELETE FROM admin_users WHERE id=?`, actor)
	})
	return aiconfig.MySQLStore{DB: db}, actor
}
func mutation(actor string) aiconfig.Mutation {
	return aiconfig.Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}
}
func revision(actor, base string, op contracts.Operation) aiconfig.Revision {
	id := uuid.NewString()
	return aiconfig.Revision{ID: id, Operation: op, BaseVersion: base, CreatedBy: actor, CreatedAt: time.Now(), Policy: contracts.ExecutionPolicy{Version: id, Endpoint: "ep-test", ReasoningEffort: "low", TimeoutSeconds: 90}}
}
func mustDraft(t *testing.T, s aiconfig.MySQLStore, actor, base string, op contracts.Operation) aiconfig.Revision {
	t.Helper()
	r, err := s.Draft(context.Background(), revision(actor, base, op), mutation(actor))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestMySQLPublishConcurrentAtomicAndIdempotent(t *testing.T) {
	s, actor := mysqlFixture(t)
	ctx := context.Background()
	op := contracts.OperationMealDecision
	first := mustDraft(t, s, actor, "", op)
	second := mustDraft(t, s, actor, "", op)
	start := make(chan struct{})
	type result struct {
		r   aiconfig.Revision
		err error
		m   aiconfig.Mutation
		p   aiconfig.Publication
	}
	done := make(chan result, 2)
	for _, draft := range []aiconfig.Revision{first, second} {
		go func(r aiconfig.Revision) {
			m := mutation(actor)
			p := aiconfig.Publication{Operation: op, Revision: r.ID}
			<-start
			published, err := s.Publish(ctx, p, m, time.Now())
			done <- result{published, err, m, p}
		}(draft)
	}
	close(start)
	var winner result
	wins, conflicts := 0, 0
	for range 2 {
		r := <-done
		if r.err == nil {
			winner = r
			wins++
		} else if errors.Is(r.err, aiconfig.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(r.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	again, err := s.Publish(ctx, winner.p, winner.m, time.Now())
	if err != nil || again.ID != winner.r.ID {
		t.Fatal("idempotency failed", err)
	}
	if n := count(t, s.DB, `SELECT COUNT(*) FROM admin_audit_events WHERE admin_user_id=?`, actor); n != 3 {
		t.Fatalf("audit count=%d, want 2 drafts plus 1 publish", n)
	}
	current, err := s.Current(ctx, op)
	if err != nil || current.ID != winner.r.ID || current.PublishedAt == nil {
		t.Fatal("publication not stored", err)
	}
	next := mustDraft(t, s, actor, current.ID, op)
	if _, err = s.Publish(ctx, aiconfig.Publication{Operation: op, Revision: next.ID, BaseVersion: current.ID}, mutation(actor), time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, err := s.Replay(ctx, winner.m, aiconfig.PublishAction, winner.p)
	if err != nil || replay.ID != winner.r.ID {
		t.Fatal("retry lost original result after newer publication", err)
	}
}
func TestMySQLDraftIdempotencyAndAuditRollback(t *testing.T) {
	s, actor := mysqlFixture(t)
	ctx := context.Background()
	op := contracts.OperationMealTextCapture
	m := mutation(actor)
	r, err := s.Draft(ctx, revision(actor, "", op), m)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Draft(ctx, revision(actor, "", op), m)
	if err != nil || duplicate.ID != r.ID {
		t.Fatal("draft retry duplicated data", err)
	}
	changed := revision(actor, "", op)
	changed.Policy.ReasoningEffort = "high"
	if _, err = s.Draft(ctx, changed, m); !errors.Is(err, aiconfig.ErrConflict) {
		t.Fatal("key accepted changed content", err)
	}
	_, err = s.DB.Exec(`CREATE TRIGGER ai_fixture_audit_failure BEFORE INSERT ON admin_audit_events FOR EACH ROW BEGIN IF NEW.action='ai.config.publish' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture audit failure'; END IF; END`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Exec(`DROP TRIGGER ai_fixture_audit_failure`)
	pm := mutation(actor)
	if _, err = s.Publish(ctx, aiconfig.Publication{Operation: op, Revision: r.ID}, pm, time.Now()); err == nil {
		t.Fatal("accepted failed audit")
	}
	if current, err := s.Current(ctx, op); err != nil || current != nil {
		t.Fatal("publication escaped rollback", err)
	}
	if n := count(t, s.DB, `SELECT COUNT(*) FROM admin_operations WHERE admin_user_id=? AND idempotency_key=?`, actor, pm.Key); n != 0 {
		t.Fatal("failed mutation left a stuck idempotency record")
	}
	stored, err := s.Get(ctx, op, r.ID)
	if err != nil || stored.PublishedAt != nil {
		t.Fatal("draft was marked published on failed audit", err)
	}
}
func TestMySQLHistoryPaginationAndDirectLookup(t *testing.T) {
	s, actor := mysqlFixture(t)
	ctx := context.Background()
	op := contracts.OperationHydrationCupEstimate
	var first aiconfig.Revision
	for i := 0; i < 105; i++ {
		r := mustDraft(t, s, actor, "", op)
		if i == 0 {
			first = r
		}
	}
	found, err := s.Get(ctx, op, first.ID)
	if err != nil || found.ID != first.ID {
		t.Fatal("old revision inaccessible", err)
	}
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := s.HistoryPage(ctx, op, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Revisions {
			if seen[r.ID] {
				t.Fatal("duplicate pagination record")
			}
			seen[r.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 105 {
		t.Fatalf("history count=%d", len(seen))
	}
	if _, err = s.Get(ctx, contracts.OperationMealDecision, first.ID); !errors.Is(err, aiconfig.ErrNotFound) {
		t.Fatal("revision crossed operation boundary")
	}
}
