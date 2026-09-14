package voice

import (
	"errors"
	"testing"

	"github.com/tellyouwhat/backend/internal/costcontrol"
)

func TestVoiceProviderAdmissionFailuresRemainActionable(t *testing.T) {
	tests := []struct {
		err  error
		code string
	}{
		{costcontrol.ErrBudgetExceeded, "voice_service_budget_exhausted"},
		{costcontrol.ErrProtectionActive, "voice_service_protected"},
		{costcontrol.ErrConfigurationConflict, "voice_service_configuration_error"},
		{costcontrol.ErrConcurrencyExceeded, "voice_session_busy"},
		{errors.New("provider unavailable"), "voice_rewrite_unavailable"},
	}
	for _, test := range tests {
		if got := voiceProviderFailureCode("voice_rewrite_unavailable", test.err); got != test.code {
			t.Errorf("error %v: got %q, want %q", test.err, got, test.code)
		}
	}
}
