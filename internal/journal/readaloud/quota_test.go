package readaloud

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/testutil"
	"github.com/tellyouwhat/backend/migrations"
)

func TestUnitRequestFingerprintBindsAllPaidInputs(t *testing.T) {
	base := validSynthesis()
	request, err := NewUnitRequest(base, "voice-revision", "journal")
	if err != nil || !request.valid() {
		t.Fatal(err)
	}
	for _, change := range []func(*Synthesis){
		func(s *Synthesis) { s.Text += "。" }, func(s *Synthesis) { s.VoiceID = "another_voice" },
		func(s *Synthesis) { s.Pitch = 1 }, func(s *Synthesis) { s.Style = "calm" },
		func(s *Synthesis) { s.Dialect = "yue" }, func(s *Synthesis) { s.Language = "en" },
	} {
		other := base
		change(&other)
		r, err := NewUnitRequest(other, "voice-revision", "journal")
		if err != nil || request.Hash == r.Hash {
			t.Fatal("changed paid input reused fingerprint", err)
		}
	}
	for _, pair := range [][2]string{{"other-revision", "journal"}, {"voice-revision", "preview"}} {
		r, err := NewUnitRequest(base, pair[0], pair[1])
		if err != nil || request.Hash == r.Hash {
			t.Fatal("scope omitted from fingerprint", err)
		}
	}
	base.RequestID = strings.ToUpper(base.RequestID)
	r, err := NewUnitRequest(base, "voice-revision", "journal")
	if err != nil || r != request {
		t.Fatal("UUID case caused different synthesis claim", err)
	}
	base.RequestID = uuid.Nil.String()
	if _, err := NewUnitRequest(base, "voice-revision", "journal"); err == nil {
		t.Fatal("nil request identity accepted")
	}
}

func quotaGrant(now time.Time) Grant {
	return Grant{Subject: Subject{"journal", "synthetic-installation"}, Purpose: "journal", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Limit: 100}
}
func TestSpeechGrantRequiresScopedCurrentServerPeriod(t *testing.T) {
	now := time.Now()
	base := quotaGrant(now)
	if !base.Valid(now) {
		t.Fatal("valid grant rejected")
	}
	for _, change := range []func(*Grant){
		func(g *Grant) { g.Subject.AppID = "health" }, func(g *Grant) { g.Subject.ID = "" },
		func(g *Grant) { g.Start = now.Add(time.Second) }, func(g *Grant) { g.End = now },
		func(g *Grant) { g.Limit = -1 }, func(g *Grant) { g.Purpose = "unscoped" },
		func(g *Grant) { g.End = now.Add(371 * 24 * time.Hour) },
	} {
		g := base
		change(&g)
		if g.Valid(now) {
			t.Fatal("invalid entitlement period admitted", g)
		}
	}
}

func TestMySQLSpeechClaimIsIdempotentAndUnknownCostSurvivesRestart(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := QuotaStore{DB: db}
	grant := quotaGrant(now)
	input := validSynthesis()
	input.Text = "hello"
	request, _ := NewUnitRequest(input, "revision", "journal")
	var group sync.WaitGroup
	replays := make(chan bool, 12)
	failures := make(chan error, 12)
	for range 12 {
		group.Go(func() {
			_, replay, err := store.Begin(ctx, grant, request, now)
			if err != nil {
				failures <- err
			}
			replays <- replay
		})
	}
	group.Wait()
	close(replays)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	claims := 0
	for replay := range replays {
		if !replay {
			claims++
		}
	}
	if claims != 1 {
		t.Fatal("multiple paid provider claims", claims)
	}
	if err := store.MarkRunning(ctx, grant.Subject, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning(ctx, grant.Subject, request.ID); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("running request reclaimed", err)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "unknown", 0, false); err != nil {
		t.Fatal(err)
	}
	store = QuotaStore{DB: db}
	r, replay, err := store.Begin(ctx, grant, request, now)
	if err != nil || !replay || r.State != "unknown" {
		t.Fatal("restart permits another synthesis", r, err)
	}
	usage, err := store.Usage(ctx, grant, now)
	if err != nil || usage.Reserved != 5 || usage.Used != 0 || usage.Remaining != 95 {
		t.Fatal("unknown charge released", usage, err)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "succeeded", 7, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "succeeded", 7, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "succeeded", 8, true); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("known settlement rewritten", err)
	}
	usage, _ = store.Usage(ctx, grant, now)
	if usage.Used != 7 || usage.Reserved != 0 {
		t.Fatal("settled more than once", usage)
	}
	grant.Limit = 8
	request.ID = uuid.NewString()
	if _, _, err := store.Begin(ctx, grant, request, now); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatal("overspending admitted", err)
	}
	// Reapplying a migration after a DDL/checkpoint interruption preserves state.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name='0068_journal_read_aloud_quota.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	usage, _ = store.Usage(ctx, grant, now)
	if usage.Used != 7 {
		t.Fatal("migration replay reset quota", usage)
	}
	grant.Limit = 100
	input.RequestID = uuid.NewString()
	input.Text = "hey"
	request, _ = NewUnitRequest(input, "revision", "journal")
	if _, _, err := store.Begin(ctx, grant, request, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "succeeded", 0, false); err != nil {
		t.Fatal(err)
	}
	r, err = store.Get(ctx, grant.Subject, request.ID)
	if err != nil || r.State != "succeeded" || r.UsageKnown {
		t.Fatal("successful audio confused with unknown provider outcome", r, err)
	}
	usage, _ = store.Usage(ctx, grant, now)
	if usage.Used != 7 || usage.Reserved != 3 {
		t.Fatal("unknown usage released for completed audio", usage)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "succeeded", 3, true); err != nil {
		t.Fatal(err)
	}
	usage, _ = store.Usage(ctx, grant, now)
	if usage.Used != 10 || usage.Reserved != 0 {
		t.Fatal("authoritative usage not reconciled", usage)
	}
}

func TestMySQLSpeechQuotaSeparatesSubjectsAndPreviewAllowance(t *testing.T) {
	db := testutil.MySQL(t)
	store := QuotaStore{DB: db}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	grant := quotaGrant(now)
	input := validSynthesis()
	input.Text = "hello"
	request, _ := NewUnitRequest(input, "revision", "journal")
	if _, _, err := store.Begin(ctx, grant, request, now); err != nil {
		t.Fatal(err)
	}
	other := grant
	other.Subject.ID = "other-installation"
	if _, _, err := store.Begin(ctx, other, request, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("request moved to another subject", err)
	}
	if _, err := store.Get(ctx, other.Subject, request.ID); !errors.Is(err, ErrRequestNotFound) {
		t.Fatal("another subject can inspect request", err)
	}
	input.Text = "changed"
	changed, _ := NewUnitRequest(input, "revision", "journal")
	if _, _, err := store.Begin(ctx, grant, changed, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("request reused for new private text", err)
	}
	preview := grant
	preview.Purpose = "preview"
	preview.Limit = 2
	input.RequestID = uuid.NewString()
	input.Text = "hi"
	previewRequest, _ := NewUnitRequest(input, "revision", "preview")
	if _, _, err := store.Begin(ctx, preview, previewRequest, now); err != nil {
		t.Fatal(err)
	}
	previewRequest.ID = uuid.NewString()
	if _, _, err := store.Begin(ctx, preview, previewRequest, now); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatal("preview spending unbounded", err)
	}
	usage, _ := store.Usage(ctx, grant, now)
	if usage.Reserved != 5 || usage.Remaining != 95 {
		t.Fatal("preview consumed journal allowance", usage)
	}
	if err := store.Finish(ctx, grant.Subject, request.ID, "cancelled", 0, true); err != nil {
		t.Fatal(err)
	}
	usage, _ = store.Usage(ctx, grant, now)
	if usage.Reserved != 0 || usage.Remaining != 100 {
		t.Fatal("pre-provider cancellation did not release confirmed zero usage", usage)
	}
}
