package main

import (
	"strings"
	"testing"
)

func TestGrantRequiresExactDeviceAndBoundedExpiry(t *testing.T) {
	for _, tc := range []struct {
		hash  string
		days  int
		valid bool
	}{
		{strings.Repeat("a", 64), 30, true}, {strings.Repeat("a", 64), 1, true}, {"", 30, false},
		{strings.Repeat("A", 64), 30, false}, {strings.Repeat("g", 64), 30, false},
		{strings.Repeat("a", 63), 30, false}, {strings.Repeat("a", 64), 31, false}, {strings.Repeat("a", 64), 0, false},
	} {
		if (validateGrant(tc.hash, tc.days) == nil) != tc.valid {
			t.Errorf("invalid validation outcome for days=%d hash length=%d", tc.days, len(tc.hash))
		}
	}
}
