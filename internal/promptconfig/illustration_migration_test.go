package promptconfig

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
	"testing"
	"time"
)

func TestMySQLIllustrationBaselinePreservesPublishedPoliciesAndHistory(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	store := Store{DB: db}
	for scope, p := range Defaults("lite", "pro", "voice", 90) {
		if err := store.Initialize(ctx, scope, p, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.Published(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := before["journal"]
	var document map[string]any
	raw, _ := json.Marshal(old)
	json.Unmarshal(raw, &document)
	journal := document["policy"].(map[string]any)["journal"].(map[string]any)
	delete(journal, "illustrationSuggestionPrompt")
	delete(journal, "illustrationGenerationPrompt")
	raw, _ = json.Marshal(document)
	if _, err = db.Exec(`UPDATE prompt_config_revisions SET document=? WHERE id=?`, raw, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DELETE FROM schema_migrations WHERE name='0015_journal_illustration_prompt_baseline.sql'`); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	after, err := store.Published(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for scope, r := range after {
		if scope != "journal" {
			a, _ := json.Marshal(before[scope])
			b, _ := json.Marshal(r)
			if string(a) != string(b) {
				t.Fatal("non-Journal policy changed", scope)
			}
			continue
		}
		if r.ID == old.ID || r.BaseVersion != old.ID || r.Policy.Validate("journal") != nil {
			t.Fatal("baseline not a new valid revision")
		}
		if r.Policy.Journal.IllustrationSuggestionPrompt != IllustrationSuggestionPrompt || r.Policy.Journal.IllustrationGenerationPrompt != IllustrationGenerationPrompt {
			t.Fatal("migration changed prior fixed instructions")
		}
		a, _ := json.Marshal(old.Policy)
		b, _ := json.Marshal(r.Policy)
		if string(a) != string(b) {
			t.Fatal("existing configuration changed")
		}
	}
	var preserved []byte
	if err = db.QueryRow(`SELECT document FROM prompt_config_revisions WHERE id=?`, old.ID).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	json.Unmarshal(preserved, &actual)
	a, _ := json.Marshal(actual)
	if string(a) != string(raw) {
		t.Fatal("historical revision was rewritten")
	}
	current := after["journal"].ID
	if _, err = db.Exec(`DELETE FROM schema_migrations WHERE name='0015_journal_illustration_prompt_baseline.sql'`); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	latest, err := store.Current(ctx, "journal")
	if err != nil || latest.ID != current {
		t.Fatal("migration replay changed baseline", err)
	}
}
