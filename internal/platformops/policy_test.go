package platformops_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
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
	for _, item := range [][2]string{{"health", "meal_text_capture"}, {"journal", "journal.organize.pro"}, {"journal", "journal.voice.rewrite"}, {"journal", "journal.illustration"}} {
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
func TestReadAloudHasIndependentOperationAndAppPause(t *testing.T) {
	p := policy()
	if err := p.Check("journal", "journal.read_aloud"); err != nil {
		t.Fatal("read-aloud is missing from Journal operations", err)
	}
	a := p.Apps["journal"]
	a.PausedOperations = []string{"journal.read_aloud"}
	p.Apps["journal"] = a
	if p.Validate() != nil || !errors.Is(p.Check("journal", "journal.read_aloud"), platformops.ErrPaused) {
		t.Fatal("read-aloud cannot be paused independently")
	}
	if p.Check("journal", "journal.voice") != nil || p.Check("health", "meal_text_capture") != nil || p.Check("health", "journal.read_aloud") == nil {
		t.Fatal("read-aloud pause crossed an operation or app boundary")
	}
	a.PausedOperations, a.Paused = nil, true
	p.Apps["journal"] = a
	if !errors.Is(p.Check("journal", "journal.read_aloud"), platformops.ErrPaused) {
		t.Fatal("Journal app pause did not pause read-aloud")
	}
}

type readAloudProvider struct{ calls int }

func (p *readAloudProvider) Synthesize(_ context.Context, _ readaloud.Synthesis, emit func([]byte) error) (readaloud.SynthesisUsage, error) {
	p.calls++
	return readaloud.SynthesisUsage{Characters: 2, UsageKnown: true, AudioBytes: 2}, emit([]byte{1, 2})
}

func TestReadAloudMySQLPauseBlocksNewPaidCallsBeforeReservation(t *testing.T) {
	s, actor, costs := fixture(t)
	provider := &readAloudProvider{}
	speaker, err := readaloud.NewMeteredSynthesizer(provider, costs, readaloud.StandardCharacterNanos)
	if err != nil {
		t.Fatal(err)
	}
	play := func() error {
		_, err := speaker.Synthesize(context.Background(), readaloud.Synthesis{RequestID: uuid.NewString(), Text: "测试", VoiceID: "zh_female_xiaohe_uranus_bigtts"}, func([]byte) error { return nil })
		return err
	}
	for _, appPause := range []bool{true, false} {
		publish(t, s, actor, func(p *platformops.Policy) {
			rules := platformops.DefaultAutomationPolicy()
			rules.Mode = "observe"
			p.Automation = &rules
			a := p.Apps["journal"]
			a.Paused = appPause
			if appPause {
				a.PausedOperations = nil
			} else {
				a.PausedOperations = []string{"journal.read_aloud"}
			}
			p.Apps["journal"] = a
		})
		if err := play(); !errors.Is(err, costcontrol.ErrPaused) || provider.calls != 0 {
			t.Fatal("published pause allowed a paid call", err, provider.calls)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_cost_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("paused read-aloud reserved money", err, count)
	}
	metrics, err := s.Metrics(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var paused int64
	for _, row := range metrics.Rejections {
		if row.AppID == "journal" && row.Operation == "journal.read_aloud" && row.Reason == "paused" {
			paused += row.Count
		}
	}
	if paused != 2 {
		t.Fatal("manual pauses were not recorded as pauses", paused)
	}
	publish(t, s, actor, func(p *platformops.Policy) {
		a := p.Apps["journal"]
		a.Paused = false
		a.PausedOperations = []string{"journal.voice"}
		p.Apps["journal"] = a
	})
	if err := play(); err != nil || provider.calls != 1 {
		t.Fatal("unrelated voice pause blocked read-aloud", err, provider.calls)
	}
	var operation, status string
	if err := s.DB.QueryRow(`SELECT operation,status FROM ai_cost_attempts`).Scan(&operation, &status); err != nil || operation != "journal.read_aloud" || status != "settled" {
		t.Fatal("read-aloud did not use its own metered operation", err, operation, status)
	}
}

func TestReadAloudMySQLAutomaticProtectionAndSingleRecoveryProbe(t *testing.T) {
	s, _, costs := fixture(t)
	ctx := context.Background()
	setCircuit := func(phase string) {
		raw, err := json.Marshal(platformops.Circuit{Phase: phase, RetryAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec(`INSERT INTO platform_ops_circuits(app_id,operation,document,updated_at) VALUES('journal','journal.read_aloud',?,?) ON DUPLICATE KEY UPDATE document=VALUES(document),updated_at=VALUES(updated_at)`, raw, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	setCircuit("open")
	provider := &readAloudProvider{}
	speaker, err := readaloud.NewMeteredSynthesizer(provider, costs, readaloud.StandardCharacterNanos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = speaker.Synthesize(ctx, readaloud.Synthesis{RequestID: uuid.NewString(), Text: "测试", VoiceID: "zh_female_xiaohe_uranus_bigtts"}, func([]byte) error { return nil })
	if !errors.Is(err, costcontrol.ErrProtectionActive) || provider.calls != 0 {
		t.Fatal("automatic protection allowed paid TTS", err, provider.calls)
	}
	setCircuit("probe")
	lease, err := costs.Reserve(ctx, "journal", "journal.read_aloud", readaloud.ResourceID, 600000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = costs.Reserve(ctx, "journal", "journal.read_aloud", readaloud.ResourceID, 600000); !errors.Is(err, costcontrol.ErrProtectionActive) {
		t.Fatal("read-aloud admitted more than one recovery probe", err)
	}
	if err = lease.Finish(ctx, 600000, true, costcontrol.Outcome{Success: true, UsageKnown: true, Model: readaloud.ResourceID}); err != nil {
		t.Fatal(err)
	}
	metrics, err := s.Metrics(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, circuit := range metrics.Automation.Circuits {
		if circuit.AppID == "journal" && circuit.Operation == "journal.read_aloud" {
			if circuit.Phase != "probe" || circuit.Successes != 1 || circuit.ProbeOccupied {
				t.Fatal("read-aloud did not finish its recovery probe", circuit)
			}
			return
		}
	}
	t.Fatal("read-aloud recovery circuit disappeared")
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
