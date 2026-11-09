package mysqlstore

import "testing"

func TestCleanupRetentionWindowsMatchPublishedMaximums(t *testing.T) {
	t.Parallel()

	if auditRetentionDays != 400 || inactiveIdentityDays != 30 {
		t.Fatalf("unexpected retention windows: audit=%d identity=%d", auditRetentionDays, inactiveIdentityDays)
	}
}
