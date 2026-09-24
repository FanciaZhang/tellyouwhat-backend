package voice

import "testing"

func TestTimelineOrderPreservesPartialKnowledge(t *testing.T) {
	for _, test := range []struct {
		name               string
		day, period        string
		approximate, valid bool
	}{
		{"contradictory time", "2026-09-24", "morning", false, false},
		{"estimated time", "2026-09-24", "morning", true, true},
		{"unknown day", "", "morning", false, true},
		{"same range", "2026-09-24", "afternoon", false, true},
		{"next day", "2026-09-25", "morning", false, true},
		{"previous day", "2026-09-23", "morning", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := timelineContextFixture()
			a, b := &s.TimelineContext[0].Events[0], &s.TimelineContext[0].Events[1]
			a.Day, a.Precision, a.Period, a.Approximate = "2026-09-24", "period", "afternoon", false
			b.Day, b.Precision, b.Period, b.Approximate = test.day, "period", test.period, test.approximate
			// b already explicitly follows a. Narrated order is not time order.
			if got := validateTimelineContext(s) == nil; got != test.valid {
				t.Fatalf("valid = %v, want %v", got, test.valid)
			}
		})
	}
}

func TestTimelineEditRejectsNewChronologicalCycle(t *testing.T) {
	s, c := timelineEditFixture()
	b := &s.TimelineContext[0].Events[1]
	b.Day, b.Precision, b.Period, b.Approximate = "2026-09-24", "period", "noon", false
	if err := validateTimelineContext(s); err != nil {
		t.Fatal("original partial order invalid", err)
	}
	// Moving a to the afternoon conflicts with b being both at noon and after a.
	if err := validateTimelineEditResult(c, s); err == nil {
		t.Fatal("accepted conflicting time correction")
	}
	c.Updates[0].Time.Approximate = true
	if err := validateTimelineEditResult(c, s); err != nil {
		t.Fatal("estimated time must remain partial", err)
	}
}
