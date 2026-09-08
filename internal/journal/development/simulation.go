package development

import (
	"errors"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"net/http"
	"time"
)

// These claims are accepted only after authenticating the separately provisioned
// developer credential. Production never trusts simulation headers. The client
// persists the original start date; reopening an app never renews a monthly mock.
func simulatedRecord(r *http.Request, now time.Time) (entitlement.Record, error) {
	id, err := uuid.Parse(r.Header.Get("X-Journal-Development-Installation"))
	if err != nil {
		return entitlement.Record{}, errors.New("invalid installation")
	}
	started, err := time.Parse(time.RFC3339, r.Header.Get("X-Journal-Development-Started-At"))
	if err != nil || started.After(now.Add(5*time.Minute)) {
		return entitlement.Record{}, errors.New("invalid subscription start")
	}
	var expires time.Time
	switch r.Header.Get("X-Journal-Development-Mode") {
	case "forced":
		expires = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	case "monthly":
		expires = nextMonth(started)
	case "expired":
		expires = started
	default:
		return entitlement.Record{}, errors.New("invalid simulation mode")
	}
	return entitlement.Record{KeyID: id.String(), StartedAt: started, ExpiresAt: expires, Environment: "development"}, nil
}

func nextMonth(t time.Time) time.Time {
	_, end := voice.Period(t, t)
	return end
}
