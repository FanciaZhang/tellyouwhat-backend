package platformops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/platformops"
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
)

func automationController(t *testing.T, s platformops.Store, now func() time.Time) *costcontrol.Controller {
	t.Helper()
	c, err := costcontrol.New(mysqlstore.NewCostControlStore(s.DB), costcontrol.Limits{MonthlyBudgetNanos: 100e9, MaxConcurrent: 50, LeaseDuration: time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func automationWindow(t *testing.T, s platformops.Store, start time.Time, failed int, cancelled bool) {
	t.Helper()
	now := start
	c := automationController(t, s, func() time.Time { return now })
	for i := 0; i < 20; i++ {
		now = start.Add(time.Duration(i*2) * time.Second)
		lease, err := c.Reserve(context.Background(), "health", "meal_text_capture", "ark", 100)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if err = lease.Finish(context.Background(), 10, true, costcontrol.Outcome{Success: i >= failed, Cancelled: cancelled, UsageKnown: true, InputTokens: 10, OutputTokens: 2}); err != nil {
			t.Fatal(err)
		}
	}
}

func automationCircuit(t *testing.T, s platformops.Store, now time.Time) platformops.AutomationCircuit {
	t.Helper()
	m, err := s.Metrics(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range m.Automation.Circuits {
		if c.AppID == "health" && c.Operation == "meal_text_capture" {
			return c
		}
	}
	t.Fatal("missing circuit")
	return platformops.AutomationCircuit{}
}

func tripAutomation(t *testing.T, s platformops.Store) time.Time {
	t.Helper()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	window := platformops.DefaultAutomationPolicy().Window()
	for i := 0; i < 2; i++ {
		automationWindow(t, s, now.Add(-window), 20, false)
		if err := s.Patrol(context.Background(), now); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			now = now.Add(window)
		}
	}
	if c := automationCircuit(t, s, now); c.Phase != "open" {
		t.Fatalf("did not protect: %+v", c)
	}
	return now
}

func TestAutomationMySQLProtectionSingleProbeAndRecovery(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	now := tripAutomation(t, s)
	if err := platformops.Check(ctx, s, "health", "meal_text_capture"); !errors.Is(err, platformops.ErrProtected) {
		t.Fatalf("gateway/worker gate: %v", err)
	}
	if err := platformops.Check(ctx, s, "health", "meal_photo_capture"); err != nil {
		t.Fatal("other function blocked", err)
	}
	var before int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := s.Patrol(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	var after int
	s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_events`).Scan(&after)
	if before != after {
		t.Fatal("replayed patrol duplicated events")
	}
	// A new Store instance resumes the persisted cooldown after a restart.
	s = platformops.Store{DB: s.DB}
	now = *automationCircuit(t, s, now).RetryAt
	if err := s.Patrol(ctx, now); err != nil {
		t.Fatal(err)
	}
	c := automationController(t, s, func() time.Time { return now })
	var group sync.WaitGroup
	leases := make(chan *costcontrol.Lease, 16)
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			lease, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", 100)
			if err == nil {
				leases <- lease
			} else {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(leases)
	close(failures)
	if len(leases) != 1 || len(failures) != 15 {
		t.Fatalf("probe concurrency: admitted=%d denied=%d", len(leases), len(failures))
	}
	for err := range failures {
		if !errors.Is(err, costcontrol.ErrProtectionActive) {
			t.Fatal(err)
		}
	}
	for lease := range leases {
		if err := lease.Finish(ctx, 10, true, costcontrol.Outcome{Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		lease, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", 100)
		if err != nil {
			t.Fatal(err)
		}
		if err = lease.Finish(ctx, 10, true, costcontrol.Outcome{Success: true}); err != nil {
			t.Fatal(err)
		}
		if err = lease.Finish(ctx, 10, true, costcontrol.Outcome{Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := automationCircuit(t, s, now); got.Phase != "closed" {
		t.Fatalf("recovery failed: %+v", got)
	}
	var active int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_incidents WHERE active_key='error_rate:health:meal_text_capture'`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("incident not resolved: %d %v", active, err)
	}
	m, err := s.Metrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m.Automation)
	if strings.Contains(string(raw), "probeAttempt") || strings.Contains(string(raw), "probeID") {
		t.Fatal("probe identifiers leaked to admin snapshot")
	}
}

func TestAutomationMySQLProbeBudgetRollbackExpiryAndObserveMode(t *testing.T) {
	s, actor, _ := fixture(t)
	ctx := context.Background()
	now := tripAutomation(t, s)
	now = *automationCircuit(t, s, now).RetryAt
	if err := s.Patrol(ctx, now); err != nil {
		t.Fatal(err)
	}
	publish(t, s, actor, func(p *platformops.Policy) { a := p.Apps["health"]; a.MonthlyBudgetNanos = 1; p.Apps["health"] = a })
	c := automationController(t, s, func() time.Time { return now })
	if _, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", 100); !errors.Is(err, costcontrol.ErrBudgetExceeded) {
		t.Fatalf("budget bypass: %v", err)
	}
	if got := automationCircuit(t, s, now); got.ProbeOccupied {
		t.Fatal("denied budget consumed probe permit")
	}
	publish(t, s, actor, func(p *platformops.Policy) { a := p.Apps["health"]; a.MonthlyBudgetNanos = 0; p.Apps["health"] = a })
	lease, err := c.Reserve(ctx, "health", "meal_text_capture", "ark", 100)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err = s.Patrol(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := automationCircuit(t, s, now); got.Phase != "open" || got.RetryAt.Sub(now) != 240*time.Second {
		t.Fatalf("probe timeout did not back off: %+v", got)
	}
	if err = lease.Finish(ctx, 10, true, costcontrol.Outcome{Success: true}); err != nil {
		t.Fatal(err)
	}
	if got := automationCircuit(t, s, now); got.Phase != "open" {
		t.Fatal("late result incorrectly recovered service")
	}
	publish(t, s, actor, func(p *platformops.Policy) { a := p.AutomationRules(); a.Mode = "observe"; p.Automation = &a })
	if _, err = c.Reserve(ctx, "health", "meal_text_capture", "ark", 100); err != nil {
		t.Fatal("observe mode retained guard", err)
	}
	now = now.Add(time.Minute)
	if err = s.Patrol(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := automationCircuit(t, s, now); got.Phase != "closed" {
		t.Fatal("disabled protection remained on dashboard")
	}
}

func TestAutomationMySQLCancelledAndLegacyOutcomesDoNotTrip(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	window := platformops.DefaultAutomationPolicy().Window()
	for i := 0; i < 3; i++ {
		automationWindow(t, s, now.Add(-window), 20, true)
		if err := s.Patrol(ctx, now); err != nil {
			t.Fatal(err)
		}
		now = now.Add(window)
	}
	if got := automationCircuit(t, s, now); got.Phase != "closed" || got.BadWindows != 0 {
		t.Fatal("cancelled calls tripped protection")
	}
	if _, err := s.DB.Exec(`UPDATE ai_cost_attempts SET cancelled=FALSE,outcome_recorded_at=NULL`); err != nil {
		t.Fatal(err)
	}
	if err := s.Patrol(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := automationCircuit(t, s, now); got.BadWindows != 0 {
		t.Fatal("legacy failures became new evidence")
	}
}

func TestAutomationMySQLHostEvidenceDeduplicatesAndRecoversWithoutPatrol(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	h := platformops.HostHealth{CheckedAt: now}
	for _, name := range []string{"health_gateway", "journal_gateway", "worker", "admin", "disk_space", "backup_freshness", "maintenance_freshness", "restore_freshness"} {
		h.Checks = append(h.Checks, platformops.HostCheck{Name: name, Passed: name != "worker"})
	}
	report := func() {
		t.Helper()
		raw, _ := json.Marshal(h)
		if err := s.RecordHostHealth(ctx, bytes.NewReader(raw), h.CheckedAt); err != nil {
			t.Fatal(err)
		}
	}
	report()
	report()
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_events WHERE rule_name='worker'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated collector event: %d %v", count, err)
	}
	h.CheckedAt = now.Add(time.Minute)
	h.Checks[2].Passed = true
	report()
	report()
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_incidents WHERE active_key='worker::'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("one repeated healthy sample resolved incident")
	}
	h.CheckedAt = h.CheckedAt.Add(time.Minute)
	report()
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM platform_ops_incidents WHERE active_key='worker::'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("host recovery missing")
	}
	// An out-of-order collector cannot overwrite the newer health snapshot.
	h.CheckedAt = now
	h.Checks[2].Passed = false
	report()
	m, err := s.Metrics(ctx, now.Add(2*time.Minute))
	if err != nil || m.Automation.Host == nil || !m.Automation.Host.Checks[2].Passed {
		t.Fatal("stale host report won", err)
	}
	h.Checks = h.Checks[:1]
	raw, _ := json.Marshal(h)
	if err = s.RecordHostHealth(ctx, bytes.NewReader(raw), now); !errors.Is(err, platformops.ErrInvalid) {
		t.Fatal("partial host report accepted")
	}
}
