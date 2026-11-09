package adminportal

import "time"

func validCodePoolCount(count int, sandbox bool) bool {
	if sandbox {
		return count == 10 || count == 100 || count == 500 || count == 1000
	}
	return count >= 500 && count <= 25000 && count%500 == 0
}

func validCodePoolExpiration(value string, optional bool, now time.Time) bool {
	if value == "" {
		return optional
	}
	if !validFutureDate(value, false, now) {
		return false
	}
	date, _ := time.Parse("2006-01-02", value)
	now = now.UTC()
	month := time.Date(now.Year(), now.Month()+6, 1, 0, 0, 0, 0, time.UTC)
	lastDay := month.AddDate(0, 1, -1).Day()
	maximum := time.Date(month.Year(), month.Month(), min(now.Day(), lastDay), 0, 0, 0, 0, time.UTC)
	return !date.After(maximum)
}
