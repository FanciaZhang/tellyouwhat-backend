package platformops_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
)

func policy() platformops.Policy {
	free := 6 * contracts.MaxFreeRecognitionSessionReservationTokens
	return platformops.Policy{MonthlyBudgetNanos: 100 * costcontrol.NanosPerCNY, MaxConcurrent: 50, BudgetWarningPercent: 80, ErrorWarningPercent: 20, SlowWarningMilliseconds: 60000, MinimumSamples: 20, Apps: map[string]platformops.AppPolicy{
		"health":  {DailyTokens: 100000, MonthlyTokens: 1000000, FreeDailyTokens: free, FreeMonthlyTokens: free * 31, FreeDailySessions: 3, PausedOperations: []string{}},
		"journal": {DailyTokens: 100000, MonthlyTokens: 1000000, VoicePeriodMinutes: 120, PausedOperations: []string{}},
	}}
}
func TestPolicyBoundariesAndAppIsolation(t *testing.T) {
	p := policy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	a := p.Apps["health"]
	a.PausedOperations = []string{"meal_photo_capture"}
	p.Apps["health"] = a
	if !errors.Is(p.Check("health", "meal_photo_capture"), platformops.ErrPaused) {
		t.Fatal("photo pause missing")
	}
	for _, item := range [][2]string{{"health", "meal_text_capture"}, {"journal", "journal.organize.pro"}, {"journal", "journal.voice.rewrite"}} {
		if err := p.Check(item[0], item[1]); err != nil {
			t.Fatal(err)
		}
	}
	if p.Check("journal", "meal_photo_capture") == nil {
		t.Fatal("cross-app operation accepted")
	}
	a.PausedOperations = []string{"journal.voice"}
	p.Apps["health"] = a
	if p.Validate() == nil {
		t.Fatal("cross-app pause accepted")
	}
	p = policy()
	a = p.Apps["health"]
	a.FreeDailySessions = 100
	p.Apps["health"] = a
	if p.Validate() == nil {
		t.Fatal("free sessions can exceed the safety token budget")
	}
}
func fixture(t *testing.T) (platformops.Store, string, *costcontrol.Controller) {
	db := testutil.MySQL(t)
	s := platformops.Store{DB: db}
	actor := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO admin_users(id,webauthn_id,display_name,role) VALUES(?,?,?,'admin')`, actor, []byte(actor), "Operations fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.Initialize(context.Background(), policy(), time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := costcontrol.New(mysqlstore.NewCostControlStore(db), costcontrol.Limits{MonthlyBudgetNanos: 100 * costcontrol.NanosPerCNY, MaxConcurrent: 50, LeaseDuration: time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s, actor, c
}
func mutation(actor string) platformops.Mutation {
	return platformops.Mutation{Actor: actor, Key: uuid.NewString(), RequestID: uuid.NewString()}
}
func publish(t *testing.T, s platformops.Store, actor string, edit func(*platformops.Policy)) platformops.Revision {
	t.Helper()
	ctx := context.Background()
	r, err := s.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edit(&r.Policy)
	d, err := s.Draft(ctx, platformops.DraftInput{BaseVersion: r.ID, Policy: r.Policy}, mutation(actor), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Publish(ctx, platformops.Publication{Revision: d.ID, BaseVersion: r.ID}, mutation(actor), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestLiveBudgetChangesPreserveSpendAndSettlement(t *testing.T) {
	s, actor, c := fixture(t)
	ctx := context.Background()
	unit := costcontrol.NanosPerCNY
	health, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", 10*unit)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := c.Reserve(ctx, "journal", "journal.organize.pro", "ark", 20*unit)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, s, actor, func(p *platformops.Policy) {
		p.MonthlyBudgetNanos = 40 * unit
		a := p.Apps["health"]
		a.MonthlyBudgetNanos = 15 * unit
		p.Apps["health"] = a
	})
	if _, err = c.Reserve(ctx, "health", "meal_text_capture", "ark", 6*unit); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("app ceiling: %v", err)
	}
	if err = health.Finish(ctx, 4*unit, true, costcontrol.Outcome{Success: true, InputTokens: 100, OutputTokens: 10, Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Reserve(ctx, "health", "meal_text_capture", "ark", 10*unit); err != nil {
		t.Fatalf("settled headroom: %v", err)
	}
	r := publish(t, s, actor, func(p *platformops.Policy) { p.MonthlyBudgetNanos = unit })
	if _, err = c.Reserve(ctx, "journal", "journal.organize.pro", "ark", 1); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("lowered ceiling: %v", err)
	}
	if err = journal.Finish(ctx, 2*unit, true, costcontrol.Outcome{Success: false}); err != nil {
		t.Fatal(err)
	}
	if err = s.Initialize(ctx, policy(), time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := s.Current(ctx)
	if err != nil || current.ID != r.ID {
		t.Fatal("restart overwrote published policy")
	}
	m, err := s.Metrics(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var settled, reserved int64
	for _, row := range m.Costs {
		settled += row.SettledNanos
		reserved += row.ReservedNanos
	}
	if settled != 6*unit || reserved != 10*unit {
		t.Fatalf("settlement lost: %+v", m.Costs)
	}
	var observed int64
	for _, row := range m.Activity {
		observed += row.Succeeded + row.Failed
	}
	if observed != 2 {
		t.Fatalf("outcomes: %+v", m.Activity)
	}
}
func TestPublicationConflictReplayAndConcurrentReservation(t *testing.T) {
	s, actor, c := fixture(t)
	ctx := context.Background()
	base, err := s.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drafts := []platformops.Revision{}
	for i := 0; i < 2; i++ {
		p := base.Policy
		p.MonthlyBudgetNanos = 3 * costcontrol.NanosPerCNY
		d, err := s.Draft(ctx, platformops.DraftInput{BaseVersion: base.ID, Policy: p}, mutation(actor), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		drafts = append(drafts, d)
	}
	type result struct {
		revision platformops.Revision
		err      error
		m        platformops.Mutation
		p        platformops.Publication
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for _, d := range drafts {
		go func(d platformops.Revision) {
			m := mutation(actor)
			p := platformops.Publication{Revision: d.ID, BaseVersion: base.ID}
			<-start
			r, err := s.Publish(ctx, p, m, time.Now())
			done <- result{r, err, m, p}
		}(d)
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		out := <-done
		if out.err == nil {
			success++
			r, err := s.Publish(ctx, out.p, out.m, time.Now())
			if err != nil || r.ID != out.revision.ID {
				t.Fatal("idempotent publication failed")
			}
			out.p.BaseVersion = r.ID
			if _, err = s.Publish(ctx, out.p, out.m, time.Now()); !errors.Is(err, platformops.ErrConflict) {
				t.Fatal("changed replay accepted")
			}
		} else if !errors.Is(out.err, platformops.ErrConflict) {
			t.Fatal(out.err)
		}
	}
	if success != 1 {
		t.Fatalf("published %d conflicting revisions", success)
	}
	admissions := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			_, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", costcontrol.NanosPerCNY)
			admissions <- err
		}()
	}
	accepted := 0
	for i := 0; i < 20; i++ {
		err := <-admissions
		if err == nil {
			accepted++
		} else if !errors.Is(err, costcontrol.ErrBudgetExceeded) {
			t.Fatal(err)
		}
	}
	if accepted != 3 {
		t.Fatalf("concurrent budget admitted %d requests", accepted)
	}
	var audits int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit_events WHERE action='operations.publish'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count %d: %v", audits, err)
	}
}
