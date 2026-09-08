package promptconfig

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/testutil"
	"testing"
	"time"
)

func TestMySQLPublicationConcurrencyHistoryAndCrossInstanceRefresh(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now()
	store := Store{DB: db}
	defaults := Defaults("lite", "pro", "voice", 90)
	for scope, p := range defaults {
		if err := store.Initialize(ctx, scope, p, now); err != nil {
			t.Fatal(err)
		}
	}
	first, second := NewCache(store), NewCache(store)
	for _, c := range []*Cache{first, second} {
		if err := c.Refresh(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	actor := uuid.NewString()
	// Use the same minimal admin identity as the production mutation foreign keys.
	_, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role,status,created_at,updated_at) VALUES(?,UNHEX(REPLACE(UUID(),'-','')),'Prompt tester','admin','active',?,?)`, actor, now, now)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Current(ctx, "journal")
	if err != nil {
		t.Fatal(err)
	}
	policy := clone(current.Policy)
	policy.Journal.Organize.Lite.MaxOutputTokens = 4096
	m := Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}
	input := DraftInput{Scope: "journal", BaseVersion: current.ID, Policy: policy}
	draft, err := store.Draft(ctx, input, m, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.Draft(ctx, input, m, now)
	if err != nil || replay.ID != draft.ID {
		t.Fatal("idempotent draft", err)
	}
	input.Policy.Journal.DefaultStyle = "lively"
	if _, err = store.Draft(ctx, input, m, now); !errors.Is(err, ErrConflict) {
		t.Fatal("key reused", err)
	}
	publication := Publication{Scope: "journal", Revision: draft.ID, BaseVersion: current.ID}
	m.Key = uuid.NewString()
	published, err := store.Publish(ctx, publication, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if published.PublishedAt == nil {
		t.Fatal("missing publication")
	}
	for _, c := range []*Cache{first, second} {
		r, _ := c.Current("journal")
		if r.ID != current.ID {
			t.Fatal("early refresh")
		}
		if err = c.Refresh(ctx, now.Add(RefreshInterval)); err != nil {
			t.Fatal(err)
		}
		r, _ = c.Current("journal")
		if r.ID != draft.ID {
			t.Fatal("cross-instance refresh missing")
		}
	}
	m.Key = uuid.NewString()
	if _, err = store.Publish(ctx, publication, m, now); !errors.Is(err, ErrConflict) {
		t.Fatal("stale publish accepted", err)
	}
	history, err := store.History(ctx, "journal", "")
	if err != nil || len(history.Revisions) != 2 {
		t.Fatal("history", err, len(history.Revisions))
	}
	m.Key = uuid.NewString()
	restored, err := store.Draft(ctx, DraftInput{Scope: "journal", BaseVersion: draft.ID, Policy: current.Policy}, m, now.Add(time.Second))
	if err != nil || restored.ID == current.ID {
		t.Fatal("history must create new version", err)
	}
}
