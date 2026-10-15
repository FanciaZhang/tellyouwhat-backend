package albums

import "time"

// Constant space even when an account has many historical purchase chains.
type cloudSubscriptionSummary struct {
	seen                                  bool
	quota                                 int64
	product                               string
	lastExpiry, activeExpiry, freshExpiry time.Time
}

func (s *cloudSubscriptionSummary) add(row cloudSubscriptionRow, now time.Time) {
	s.seen = true
	if row.Expiry.After(s.lastExpiry) {
		s.lastExpiry = row.Expiry.UTC()
	}
	if !row.Active || !now.Before(row.Expiry) {
		return
	}
	fresh := minTime(row.Expiry.UTC(), row.VerifiedAt.UTC().Add(albumSubscriptionFreshness))
	if row.Quota > s.quota {
		s.quota, s.product, s.activeExpiry, s.freshExpiry = row.Quota, row.Product, row.Expiry.UTC(), fresh
	} else if row.Quota == s.quota {
		s.activeExpiry = maxTime(s.activeExpiry, row.Expiry.UTC())
		s.freshExpiry = maxTime(s.freshExpiry, fresh)
	}
}

func (s cloudSubscriptionSummary) apply(a *AlbumCloudAccount, now time.Time, verify, retention bool, limit int64) {
	if !s.lastExpiry.IsZero() {
		a.State, a.ExpiresAt = "expired", &s.lastExpiry
		if retention {
			deadline := albumRetentionDeadline(s.lastExpiry)
			a.RetentionDeadline = &deadline
			if !now.Before(deadline) {
				a.State = "retention_due"
			}
		}
	}
	if s.quota > 0 {
		a.State, a.QuotaBytes, a.ProductID, a.ExpiresAt = "active", s.quota, s.product, &s.activeExpiry
		if limit > 0 {
			a.QuotaBytes = min(a.QuotaBytes, limit)
		}
		if verify {
			a.VerificationExpiresAt = &s.freshExpiry
			a.VerificationPending = !now.Before(s.freshExpiry)
		}
	}
}

func applyProductionCloudEntitlements(a *AlbumCloudAccount, production, sandbox cloudSubscriptionSummary, now time.Time, sandboxLimit int64) {
	// Sandbox cannot replace paid rights, renew a paid retention deadline, or
	// turn a paid cancellation into free storage, even after that purchase expires.
	if production.seen {
		production.apply(a, now, true, true, 0)
		return
	}
	// Accelerated Sandbox expiry stops new uploads, never authorizes deletion.
	sandbox.apply(a, now, true, false, sandboxLimit)
}
