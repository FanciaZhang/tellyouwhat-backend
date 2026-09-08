package platformops

import "time"

// AutomationPolicy controls service-side monitoring and bounded recovery.
type AutomationPolicy struct {
	Mode                      string `json:"mode"`
	WindowMinutes             int    `json:"windowMinutes"`
	TriggerWindows            int    `json:"triggerWindows"`
	MinimumSamples            int    `json:"minimumSamples"`
	FailurePercent            int    `json:"failurePercent"`
	CooldownSeconds           int    `json:"cooldownSeconds"`
	MaxCooldownSeconds        int    `json:"maxCooldownSeconds"`
	RecoverySuccesses         int    `json:"recoverySuccesses"`
	QueueAgeMinutes           int    `json:"queueAgeMinutes"`
	QueueCount                int    `json:"queueCount"`
	UnknownReservationMinutes int    `json:"unknownReservationMinutes"`
}

func DefaultAutomationPolicy() AutomationPolicy {
	return AutomationPolicy{Mode: "protect", WindowMinutes: 5, TriggerWindows: 2, MinimumSamples: 20,
		FailurePercent: 60, CooldownSeconds: 120, MaxCooldownSeconds: 1800, RecoverySuccesses: 3,
		QueueAgeMinutes: 5, QueueCount: 10, UnknownReservationMinutes: 30}
}

func (p Policy) AutomationRules() AutomationPolicy {
	if p.Automation == nil {
		return DefaultAutomationPolicy()
	}
	return *p.Automation
}

func (p AutomationPolicy) Validate() error {
	if (p.Mode != "observe" && p.Mode != "protect") || p.WindowMinutes < 1 || p.WindowMinutes > 30 ||
		p.TriggerWindows < 2 || p.TriggerWindows > 10 || p.MinimumSamples < 10 || p.MinimumSamples > 10000 ||
		p.FailurePercent < 20 || p.FailurePercent > 100 || p.CooldownSeconds < 30 || p.CooldownSeconds > 1800 ||
		p.MaxCooldownSeconds < p.CooldownSeconds || p.MaxCooldownSeconds > 7200 ||
		p.RecoverySuccesses < 1 || p.RecoverySuccesses > 20 || p.QueueAgeMinutes < 1 || p.QueueAgeMinutes > 120 ||
		p.QueueCount < 1 || p.QueueCount > 10000 || p.UnknownReservationMinutes < 1 || p.UnknownReservationMinutes > 1440 {
		return ErrInvalid
	}
	return nil
}

func (p AutomationPolicy) Window() time.Duration { return time.Duration(p.WindowMinutes) * time.Minute }

type Circuit struct {
	Phase      string    `json:"phase"`
	LastWindow time.Time `json:"lastWindow"`
	Baseline   time.Time `json:"baseline"`
	BadWindows int       `json:"badWindows"`
	OpenCount  int       `json:"openCount"`
	RetryAt    time.Time `json:"retryAt"`
	Successes  int       `json:"successes"`
	ProbeID    string    `json:"-"`
	ProbeUntil time.Time `json:"probeUntil"`
	ChangedAt  time.Time `json:"changedAt"`
}

type AutomationSample struct {
	Start, End        time.Time
	Completed, Failed int64
}

// Observe evaluates each complete, non-overlapping window at most once.
func (c *Circuit) Observe(p AutomationPolicy, sample AutomationSample, now time.Time) string {
	if sample.End.After(now) || !sample.End.After(c.LastWindow) || sample.Start.Before(c.Baseline) || sample.End.Sub(sample.Start) != p.Window() {
		return ""
	}
	if !c.LastWindow.IsZero() && !sample.Start.Equal(c.LastWindow) {
		c.BadWindows = 0
	}
	c.LastWindow = sample.End
	if c.Phase != "" && c.Phase != "closed" {
		return ""
	}
	bad := sample.Completed >= int64(p.MinimumSamples) && sample.Failed*100 >= sample.Completed*int64(p.FailurePercent)
	if !bad {
		c.BadWindows = 0
		return ""
	}
	c.BadWindows++
	if c.BadWindows >= p.TriggerWindows && p.Mode == "protect" {
		c.open(p, now)
		return "protected"
	}
	return ""
}

func (c *Circuit) open(p AutomationPolicy, now time.Time) {
	c.OpenCount = min(c.OpenCount+1, 16)
	delay := min(p.CooldownSeconds*(1<<(c.OpenCount-1)), p.MaxCooldownSeconds)
	c.Phase, c.RetryAt, c.ChangedAt = "open", now.Add(time.Duration(delay)*time.Second), now
	c.ProbeID, c.ProbeUntil, c.Successes, c.BadWindows = "", time.Time{}, 0, 0
}

// Advance also recovers a probe whose owning request disappeared.
func (c *Circuit) Advance(p AutomationPolicy, now time.Time) string {
	if p.Mode == "observe" {
		if c.Phase == "open" || c.Phase == "probe" {
			*c = Circuit{Phase: "closed", Baseline: now, ChangedAt: now}
			return "protection_disabled"
		}
		return ""
	}
	if c.Phase == "probe" && c.ProbeID != "" && !now.Before(c.ProbeUntil) {
		c.open(p, now)
		return "probe_expired"
	}
	if c.Phase == "open" && !now.Before(c.RetryAt) {
		c.Phase, c.ChangedAt = "probe", now
		return "probe_ready"
	}
	return ""
}

func (c Circuit) Permits(now time.Time) bool {
	return c.Phase == "" || c.Phase == "closed" || (c.Phase == "probe" && c.ProbeID == "")
}

// Claim must commit atomically with the normal cost and concurrency reservation.
func (c *Circuit) Claim(id string, expires time.Time) {
	if c.Phase == "probe" {
		c.ProbeID, c.ProbeUntil = id, expires
	}
}

// Complete only accepts the currently owned probe; late and replayed results do not recover it.
func (c *Circuit) Complete(p AutomationPolicy, id, result string, now time.Time) string {
	if c.Phase != "probe" || c.ProbeID == "" || c.ProbeID != id || !now.Before(c.ProbeUntil) {
		return ""
	}
	c.ProbeID, c.ProbeUntil = "", time.Time{}
	if result == "cancelled" {
		return "probe_cancelled"
	}
	if result != "success" {
		c.open(p, now)
		return "probe_failed"
	}
	c.Successes++
	if c.Successes >= p.RecoverySuccesses {
		*c = Circuit{Phase: "closed", Baseline: now, ChangedAt: now}
		return "recovered"
	}
	return "probe_succeeded"
}
