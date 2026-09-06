package quota

import (
	"errors"
	"time"

	"github.com/tellyouwhat/backend/internal/contracts"
)

var ErrInvalidReservation = errors.New("quota reservation is unavailable or does not match")

// TokenReservation binds a later adjustment to the counters that admitted it.
// It contains accounting metadata only, without prompts or response content.
type TokenReservation struct {
	Version           int    `json:"version"`
	TransactionID     string `json:"transactionID"`
	DeviceID          string `json:"deviceID"`
	DailyWindow       string `json:"dailyWindow"`
	MonthlyWindow     string `json:"monthlyWindow"`
	ReservedTokens    int    `json:"reservedTokens"`
	Reconciled        bool   `json:"reconciled"`
	RootReservationID string `json:"rootReservationID,omitempty"`
	Attempt           int    `json:"attempt,omitempty"`
}

func (value TokenReservation) Matches(transactionID string, reserved int) bool {
	if value.Version != 1 || value.DeviceID == "" || transactionID == "" || value.TransactionID != transactionID ||
		reserved < 0 || value.ReservedTokens != reserved {
		return false
	}
	day, err := time.Parse("20060102", value.DailyWindow)
	return err == nil && day.Format("200601") == value.MonthlyWindow
}

func JobReservationID(keyID, requestID, bodyDigest string) string {
	return contracts.BodySHA256([]byte("job-capability\n" + keyID + "\n" + requestID + "\n" + bodyDigest))
}
