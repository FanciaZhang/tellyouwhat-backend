package voice

import (
	"testing"
	"time"
)

func TestVoiceSessionAllowsFinalizationWithoutExtendingEntitlement(t *testing.T) {
	start := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	deadline := voiceSessionDeadline(start, start.Add(time.Hour))
	if !deadline.After(start.Add(35*time.Minute)) || deadline.After(start.Add(40*time.Minute)) {
		t.Fatal("30-minute input must retain bounded time for transport and final organization")
	}
	for _, remaining := range []time.Duration{-time.Second, 20 * time.Minute, 37 * time.Minute} {
		expiry := start.Add(remaining)
		if got := voiceSessionDeadline(start, expiry); !got.Equal(expiry) {
			t.Fatal("session outlived its actual entitlement")
		}
	}
}
