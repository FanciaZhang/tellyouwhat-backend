package albums

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// Development capacity is an operator grant, never a synthetic StoreKit purchase.
type DevelopmentCloudConfig struct {
	quota     int64
	owners    map[string]struct{}
	expiresAt time.Time
}

func (DevelopmentCloudConfig) String() string   { return "[redacted development cloud grant]" }
func (DevelopmentCloudConfig) GoString() string { return "[redacted development cloud grant]" }

func loadDevelopmentCloudConfig(getenv func(string) string) (*DevelopmentCloudConfig, error) {
	quota := strings.TrimSpace(getenv("ALBUM_DEVELOPMENT_CLOUD_QUOTA_BYTES"))
	owners := strings.TrimSpace(getenv("ALBUM_DEVELOPMENT_CLOUD_ACCOUNTS"))
	expires := strings.TrimSpace(getenv("ALBUM_DEVELOPMENT_CLOUD_EXPIRES_AT"))
	if quota == "" && owners == "" && expires == "" {
		return nil, nil
	}
	if getenv("ALBUM_DEPLOYMENT_ENVIRONMENT") != "test" ||
		(strings.TrimSpace(getenv("ALBUM_APPLE_BUNDLE_ID")) != "" && strings.TrimSpace(getenv("ALBUM_APPLE_BUNDLE_ID")) != albumDevelopmentAudience) ||
		(strings.TrimSpace(getenv("ALBUM_SUBSCRIPTIONS_ENABLED")) != "" && strings.TrimSpace(getenv("ALBUM_SUBSCRIPTIONS_ENABLED")) != "false") {
		return nil, errors.New("development cloud grants require the test identity with purchases disabled")
	}
	db, err := mysql.ParseDSN(getenv("ALBUM_DATABASE_DSN"))
	if err != nil || !strings.HasSuffix(db.User, "_test") || !albumTestDatabasePattern.MatchString(db.DBName) {
		return nil, errors.New("development cloud grants require a test database and test-only account")
	}
	c := &DevelopmentCloudConfig{owners: make(map[string]struct{})}
	c.quota, err = strconv.ParseInt(quota, 10, 64)
	if err != nil || c.quota < 1 || c.quota > 1_000_000_000 {
		return nil, errors.New("development cloud quota must be between 1 and 1000000000 bytes")
	}
	c.expiresAt, err = time.Parse(time.RFC3339, expires)
	if err != nil || c.expiresAt.IsZero() {
		return nil, errors.New("development cloud grants require an explicit expiration")
	}
	c.expiresAt = c.expiresAt.UTC()
	if len(owners) > 512 {
		return nil, errors.New("development cloud grants allow at most three accounts")
	}
	for _, owner := range strings.Split(owners, ",") {
		owner = strings.TrimSpace(owner)
		id, err := uuid.Parse(owner)
		if err != nil || id == uuid.Nil || id.String() != owner {
			return nil, errors.New("development cloud grants require canonical account IDs")
		}
		if _, exists := c.owners[owner]; exists {
			return nil, errors.New("duplicate development cloud account")
		}
		c.owners[owner] = struct{}{}
	}
	if len(c.owners) > 3 {
		return nil, errors.New("development cloud grants allow at most three accounts")
	}
	return c, nil
}

func (r *MySQLUploads) WithDevelopmentCloud(c *DevelopmentCloudConfig) *MySQLUploads {
	if c != nil {
		r.developmentCloud = c
		// Existing quota locking is shared with billing, but no purchase is invented.
		r.WithSubscriptionEnvironment("Sandbox")
	}
	return r
}

func (c *DevelopmentCloudConfig) apply(a *AlbumCloudAccount, now time.Time) {
	a.QuotaKind = "development"
	if _, allowed := c.owners[a.AccountID]; !allowed {
		return
	}
	a.ExpiresAt = &c.expiresAt
	a.State = "expired"
	if !now.Before(c.expiresAt) {
		return
	}
	a.State, a.QuotaBytes = "active", c.quota
	until := minTime(c.expiresAt, now.Add(5*time.Minute))
	a.VerificationExpiresAt = &until
}
