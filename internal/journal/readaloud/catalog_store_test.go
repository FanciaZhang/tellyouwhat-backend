package readaloud

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/aiconfig"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func catalogTestStore(t *testing.T) (CatalogStore, aiconfig.Mutation, time.Time) {
	t.Helper()
	db := testutil.MySQL(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	actor := uuid.NewString()
	_, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role,status,created_at,updated_at) VALUES(?,UNHEX(REPLACE(UUID(),'-','')),'Voice catalog tester','admin','active',?,?)`, actor, now, now)
	if err != nil {
		t.Fatal(err)
	}
	return CatalogStore{DB: db}, aiconfig.Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}, now
}
func anotherMutation(m aiconfig.Mutation) aiconfig.Mutation {
	m.Key = uuid.NewString()
	m.RequestID = uuid.NewString()
	return m
}

func TestMySQLVoiceCatalogPublicationRollbackAndImmutableHistory(t *testing.T) {
	store, m, now := catalogTestStore(t)
	ctx := context.Background()
	v := draftVoice()
	v.Enabled = true
	input := CatalogDraft{DefaultVoiceID: v.ID, Voices: []VoiceConfiguration{Configuration(v)}}
	first, err := store.Draft(ctx, input, m, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(ctx); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatal("draft published implicitly", err)
	}
	if _, err := store.Publish(ctx, CatalogPublication{Revision: first.Version}, anotherMutation(m), now); !errors.Is(err, ErrCatalog) {
		t.Fatal("unverified voice published", err)
	}
	probeMutation := anotherMutation(m)
	if previous, err := store.ClaimProbe(ctx, probeMutation, v, now); err != nil || previous != nil {
		t.Fatal("cannot claim probe", err)
	}
	verified, _, err := VerifyVoice(ctx, v, &catalogSynthesizer{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishProbe(ctx, probeMutation, verified, true, now); err != nil {
		t.Fatal(err)
	}
	if previous, err := store.ClaimProbe(ctx, probeMutation, v, now); err != nil || previous == nil || !previous.IsVerified() {
		t.Fatal("successful probe not replayed", err)
	}
	m = anotherMutation(m)
	second, err := store.Draft(ctx, input, m, now)
	if err != nil || !second.Voices[0].IsVerified() {
		t.Fatal("server verification not resolved", err)
	}
	if replay, err := store.Draft(ctx, input, m, now); err != nil || replay.Version != second.Version {
		t.Fatal("duplicate draft not replayed", err)
	}
	changed := input
	changed.DefaultVoiceID = "different"
	if _, err := store.Draft(ctx, changed, m, now); !errors.Is(err, ErrCatalogConflict) {
		t.Fatal("same key accepted new configuration", err)
	}
	publication := CatalogPublication{Revision: second.Version}
	m = anotherMutation(m)
	if _, err := store.Publish(ctx, publication, m, now); err != nil {
		t.Fatal(err)
	}
	if replay, err := store.Publish(ctx, publication, m, now); err != nil || replay.PublishedAt == nil {
		t.Fatal("publication replay failed", err)
	}
	input.BaseVersion = second.Version
	input.Voices[0].Name = "新的展示名称"
	input.Voices[0].Rank = 5
	candidate := draftVoice()
	candidate.ID = "candidate_voice"
	candidate.BillingClass = "unreviewed"
	input.Voices = append(input.Voices, Configuration(candidate))
	third, err := store.Draft(ctx, input, anotherMutation(m), now)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Voices[0].IsVerified() {
		t.Fatal("display change required unnecessary paid verification")
	}
	if _, err := store.Publish(ctx, CatalogPublication{Revision: third.Version, BaseVersion: second.Version}, anotherMutation(m), now); err != nil {
		t.Fatal(err)
	}
	rollback := CatalogDraft{BaseVersion: third.Version, DefaultVoiceID: second.DefaultVoiceID, Voices: []VoiceConfiguration{Configuration(second.Voices[0])}}
	fourth, err := store.Draft(ctx, rollback, anotherMutation(m), now)
	if err != nil || fourth.Version == second.Version {
		t.Fatal("rollback did not create a new revision", err)
	}
	if _, err := store.Publish(ctx, CatalogPublication{Revision: fourth.Version, BaseVersion: third.Version}, anotherMutation(m), now); err != nil {
		t.Fatal(err)
	}
	current, err := (CatalogStore{DB: store.DB}).Current(ctx)
	if err != nil || current.Version != fourth.Version {
		t.Fatal("other instance did not observe publication", err)
	}
	public, err := current.Public()
	if err != nil || len(public.Voices) != 1 || public.Voices[0].Name != v.Name {
		t.Fatal("rollback/public projection incorrect", err)
	}
	history, err := store.History(ctx, "")
	if err != nil || len(history.Revisions) != 4 {
		t.Fatal("history incomplete", err, len(history.Revisions))
	}
	expectedCounts := map[string]int{first.Version: 1, second.Version: 1, third.Version: 2, fourth.Version: 1}
	for _, row := range history.Revisions {
		if row.VoiceCount != expectedCounts[row.Version] || row.EnabledCount != 1 || row.CreatedBy != m.Actor {
			t.Fatal("invalid history summary", row)
		}
	}
	page, err := store.History(ctx, history.Revisions[0].Version)
	if err != nil || len(page.Revisions) != 3 {
		t.Fatal("history cursor duplicated or omitted rows", err, page)
	}
	original, err := store.Get(ctx, first.Version)
	if err != nil || original.PublishedAt != nil || original.Voices[0].Verification != nil {
		t.Fatal("historical draft rewritten", err)
	}
	var audits int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit_events WHERE app_id='journal'`).Scan(&audits); err != nil || audits != 9 {
		t.Fatal("missing or duplicated mutation audit", audits, err)
	}
}

func TestMySQLVoiceCatalogConcurrentProbeAndFailedProbeNeverReplaysPaidCalls(t *testing.T) {
	store, m, now := catalogTestStore(t)
	ctx := context.Background()
	v := draftVoice()
	var group sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		group.Go(func() { _, err := store.ClaimProbe(ctx, m, v, now); results <- err })
	}
	group.Wait()
	close(results)
	claims := 0
	for err := range results {
		if err == nil {
			claims++
		} else if !errors.Is(err, ErrCatalogConflict) {
			t.Fatal(err)
		}
	}
	if claims != 1 {
		t.Fatal("more than one paid probe admitted", claims)
	}
	if err := store.FinishProbe(ctx, m, v, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimProbe(ctx, m, v, now); !errors.Is(err, ErrCatalogConflict) {
		t.Fatal("failed/unknown probe bought again", err)
	}
	input := CatalogDraft{DefaultVoiceID: v.ID, Voices: []VoiceConfiguration{Configuration(v)}}
	r, err := store.Draft(ctx, input, anotherMutation(m), now)
	if err != nil || r.Voices[0].Verification != nil {
		t.Fatal("failed probe became trusted", err)
	}
	input.Voices[0].ID = "duplicated"
	input.Voices = append(input.Voices, input.Voices[0])
	if _, err := store.Draft(ctx, input, anotherMutation(m), now); !errors.Is(err, ErrCatalog) {
		t.Fatal("duplicate voice IDs admitted", err)
	}
}

func TestMySQLVoiceCatalogProbeUsesSharedDailyQuotaAndReplaysWithoutProvider(t *testing.T) {
	store, m, now := catalogTestStore(t)
	ctx := context.Background()
	provider := &catalogSynthesizer{}
	service := CatalogProbe{Store: store, Quotas: QuotaStore{DB: store.DB}, Provider: provider, DailyCharacters: 1000, Now: func() time.Time { return now }}
	input := Configuration(draftVoice())
	result, err := service.Verify(ctx, input, m)
	if err != nil || !result.Voice.IsVerified() || len(result.Audio) == 0 {
		t.Fatal("probe failed", err)
	}
	calls := len(provider.calls)
	if replay, err := service.Verify(ctx, input, m); err != nil || !replay.Replayed || len(provider.calls) != calls {
		t.Fatal("probe replay called provider", err)
	}
	service.DailyCharacters = 1
	if _, err := service.Verify(ctx, input, anotherMutation(m)); !errors.Is(err, ErrQuotaExceeded) || len(provider.calls) != calls {
		t.Fatal("daily verification budget bypassed", err)
	}
	input.Enabled = true
	r, err := store.Draft(ctx, CatalogDraft{DefaultVoiceID: input.ID, Voices: []VoiceConfiguration{input}}, anotherMutation(m), now)
	if err != nil || !r.Voices[0].IsVerified() {
		t.Fatal("verified capability unavailable to later drafts", err)
	}
	input.Dialects = append(input.Dialects, "beijing")
	r, err = store.Draft(ctx, CatalogDraft{DefaultVoiceID: input.ID, Voices: []VoiceConfiguration{input}}, anotherMutation(m), now)
	if err != nil || r.Voices[0].IsVerified() {
		t.Fatal("new untested capability reused old proof", err)
	}
}
