package platformops_test

import (
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/platformops"
)

func TestAutomationCircuitRequiresDistinctCompleteWindows(t *testing.T) {
	p := platformops.DefaultAutomationPolicy()
	end := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := platformops.Circuit{}
	sample := platformops.AutomationSample{Start: end.Add(-p.Window()), End: end, Completed: 20, Failed: 12}
	if got := c.Observe(p, sample, end); got != "" || c.BadWindows != 1 {
		t.Fatalf("first window: %+v %s", c, got)
	}
	for i := 0; i < 10; i++ {
		c.Observe(p, sample, end)
	}
	if c.BadWindows != 1 || !c.Permits(end) {
		t.Fatal("replayed window triggered protection")
	}
	sample.Start, sample.End = end, end.Add(p.Window())
	c.Observe(p, sample, end)
	if c.BadWindows != 1 {
		t.Fatal("incomplete window counted")
	}
	if got := c.Observe(p, sample, sample.End); got != "protected" || c.Permits(sample.End) {
		t.Fatalf("protection: %+v %s", c, got)
	}
	if got := c.Advance(p, sample.End.Add(time.Second)); got != "" {
		t.Fatal("cooldown was skipped")
	}
	if got := c.Advance(p, c.RetryAt); got != "probe_ready" || !c.Permits(c.RetryAt) {
		t.Fatal("probe did not become available")
	}
}

func TestAutomationCircuitSparseAndMissingWindowsDoNotAccumulate(t *testing.T) {
	p := platformops.DefaultAutomationPolicy()
	end := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, sparse := range []bool{true, false} {
		c := platformops.Circuit{}
		c.Observe(p, platformops.AutomationSample{Start: end.Add(-p.Window()), End: end, Completed: 20, Failed: 20}, end)
		if sparse {
			c.Observe(p, platformops.AutomationSample{Start: end, End: end.Add(p.Window()), Completed: 2, Failed: 2}, end.Add(p.Window()))
		}
		at := end.Add(2 * p.Window())
		c.Observe(p, platformops.AutomationSample{Start: at.Add(-p.Window()), End: at, Completed: 20, Failed: 20}, at)
		if c.BadWindows != 1 || !c.Permits(at) {
			t.Fatalf("missing samples treated as consecutive failure: %+v", c)
		}
	}
}

func TestAutomationProbeRecoveryTimeoutAndLateResults(t *testing.T) {
	p := platformops.DefaultAutomationPolicy()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := platformops.Circuit{Phase: "probe", OpenCount: 1}
	c.Claim("first", now.Add(time.Minute))
	if c.Permits(now) {
		t.Fatal("claimed probe permits concurrent requests")
	}
	if got := c.Complete(p, "wrong", "success", now); got != "" || c.ProbeID != "first" {
		t.Fatal("unowned result changed probe")
	}
	if got := c.Advance(p, now.Add(time.Minute)); got != "probe_expired" || c.Phase != "open" {
		t.Fatal("lost probe never expired")
	}
	if c.RetryAt.Sub(now.Add(time.Minute)) != 240*time.Second {
		t.Fatal("retry cooldown did not increase")
	}
	if got := c.Complete(p, "first", "success", now.Add(2*time.Minute)); got != "" || c.Phase != "open" {
		t.Fatal("late success released protection")
	}
	now = c.RetryAt
	c.Advance(p, now)
	c.Claim("cancel", now.Add(time.Minute))
	if got := c.Complete(p, "cancel", "cancelled", now); got != "probe_cancelled" || c.Successes != 0 || c.Phase != "probe" {
		t.Fatal("cancellation was counted as provider health")
	}
	for i, id := range []string{"one", "two", "three"} {
		c.Claim(id, now.Add(time.Minute))
		got := c.Complete(p, id, "success", now)
		if i < 2 && (got != "probe_succeeded" || c.Phase != "probe") {
			t.Fatal("recovered too early")
		}
		if c.Complete(p, id, "success", now) != "" {
			t.Fatal("duplicate result counted twice")
		}
	}
	if c.Phase != "closed" || !c.Baseline.Equal(now) || c.OpenCount != 0 {
		t.Fatalf("recovery: %+v", c)
	}
	// A prior failed window must not re-open a successfully recovered circuit.
	c.Observe(p, platformops.AutomationSample{Start: now.Add(-p.Window()), End: now, Completed: 30, Failed: 30}, now)
	if c.BadWindows != 0 {
		t.Fatal("pre-recovery samples were reused")
	}
}

func TestAutomationObserveModeAndBoundedCooldown(t *testing.T) {
	p := platformops.DefaultAutomationPolicy()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c := platformops.Circuit{Phase: "probe", OpenCount: 16, ProbeID: "failed", ProbeUntil: now.Add(time.Minute)}
	c.Complete(p, "failed", "error", now)
	if c.RetryAt.Sub(now) != time.Duration(p.MaxCooldownSeconds)*time.Second {
		t.Fatal("unbounded cooldown")
	}
	p.Mode = "observe"
	if c.Advance(p, now) != "protection_disabled" || !c.Permits(now) {
		t.Fatal("observation mode kept automatic blocking")
	}
	for i := 0; i < 10; i++ {
		start := now.Add(time.Duration(i) * p.Window())
		c.Observe(p, platformops.AutomationSample{Start: start, End: start.Add(p.Window()), Completed: 20, Failed: 20}, start.Add(p.Window()))
	}
	if !c.Permits(now) {
		t.Fatal("observation mode blocked requests")
	}
	base := policy()
	base.Automation = &p
	a := base.Apps["health"]
	a.Paused = true
	base.Apps["health"] = a
	if base.Check("health", "meal_photo_capture") == nil {
		t.Fatal("automation disabled manual pause")
	}
}

func TestAutomationPolicyRejectsUnsafeConfiguration(t *testing.T) {
	for _, edit := range []func(*platformops.AutomationPolicy){
		func(p *platformops.AutomationPolicy) { p.Mode = "retry_forever" },
		func(p *platformops.AutomationPolicy) { p.MinimumSamples = 1 },
		func(p *platformops.AutomationPolicy) { p.TriggerWindows = 1 },
		func(p *platformops.AutomationPolicy) { p.CooldownSeconds = 0 },
		func(p *platformops.AutomationPolicy) { p.MaxCooldownSeconds = p.CooldownSeconds - 1 },
		func(p *platformops.AutomationPolicy) { p.RecoverySuccesses = 0 },
	} {
		p := platformops.DefaultAutomationPolicy()
		edit(&p)
		if p.Validate() == nil {
			t.Fatalf("invalid automation accepted: %+v", p)
		}
	}
}
