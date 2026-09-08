package adminportal

import (
	"testing"
	"time"
)

func TestCodePoolCountsMatchAppleValidation(t *testing.T) {
	for _, count := range []int{500, 1000, 1500, 2500, 25000} {
		if !validCodePoolCount(count, false) {
			t.Errorf("valid production quantity rejected: %d", count)
		}
	}
	for _, count := range []int{-1, 0, 10, 100, 499, 501, 999, 25001, 25500} {
		if validCodePoolCount(count, false) {
			t.Errorf("invalid production quantity accepted: %d", count)
		}
	}
	for _, count := range []int{10, 100, 500, 1000} {
		if !validCodePoolCount(count, true) {
			t.Errorf("valid sandbox quantity rejected: %d", count)
		}
	}
	for _, count := range []int{1, 11, 20, 50, 501, 1500, 2000, 5000, 10000, 25000} {
		if validCodePoolCount(count, true) {
			t.Errorf("invalid sandbox quantity accepted: %d", count)
		}
	}
}

func TestCodePoolExpirationUsesSixCalendarMonths(t *testing.T) {
	for _, tc := range []struct {
		now, date       string
		optional, valid bool
	}{
		{"2026-09-08", "2026-10-08", false, true},
		{"2026-09-08", "2026-09-08", false, false},
		{"2026-09-08", "2026-03-08", false, false},
		{"2026-09-08", "2027-03-08", false, true},
		{"2026-09-08", "2027-03-09", false, false},
		{"2026-08-31", "2027-02-28", false, true},
		{"2026-08-31", "2027-03-01", false, false},
		{"2027-08-31", "2028-02-29", false, true},
		{"2026-09-08", "", true, true},
		{"2026-09-08", "", false, false},
		{"2026-09-08", "invalid", true, false},
	} {
		now, _ := time.Parse("2006-01-02", tc.now)
		if got := validCodePoolExpiration(tc.date, tc.optional, now); got != tc.valid {
			t.Errorf("now=%s date=%s optional=%v: got %v", tc.now, tc.date, tc.optional, got)
		}
	}
}
