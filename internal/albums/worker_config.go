package albums

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// WorkerConfig is deliberately separate from the managed-AI worker settings.
// No AI product, provider key, or device identity is required for album storage.
type WorkerConfig struct {
	Environment         string
	DevelopmentCloud    *DevelopmentCloudConfig
	DatabaseDSN         string
	COS                 COSConfig
	Policy              UploadPolicy
	BatchSize           int
	PollInterval        time.Duration
	PhotoProcessing     bool
	VideoProcessing     bool
	DerivedCleanup      bool
	AccountMediaErasure bool
	VideoQueueID        string
}

const (
	albumTestBucket       = "tellyouwhat-albums-1251995834"
	albumProductionBucket = "tellyouwhat-albums-production-1251995834"
	albumTestVideoQueue   = "p543daa336f7a4d4b896654b933f8881b"
)

var albumTestDatabasePattern = regexp.MustCompile(`^albums(?:_[a-z0-9]+)*_test$`)

func LoadWorkerConfig(getenv func(string) string) (WorkerConfig, error) {
	var c WorkerConfig
	if getenv == nil {
		return c, errors.New("album environment reader is required")
	}
	required := func(key string) (string, error) {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			return "", errors.New("missing " + key)
		}
		return value, nil
	}
	fields := []struct {
		key    string
		target *string
	}{
		{"ALBUM_DATABASE_DSN", &c.DatabaseDSN},
		{"ALBUM_COS_BUCKET", &c.COS.Bucket}, {"ALBUM_COS_REGION", &c.COS.Region},
		{"ALBUM_COS_ARCHIVE_SECRET_ID", &c.COS.SecretID}, {"ALBUM_COS_ARCHIVE_SECRET_KEY", &c.COS.SecretKey},
		{"ALBUM_COS_UPLOAD_SECRET_ID", &c.COS.UploadSecretID}, {"ALBUM_COS_UPLOAD_SECRET_KEY", &c.COS.UploadSecretKey},
	}
	for _, field := range fields {
		value, err := required(field.key)
		if err != nil {
			return WorkerConfig{}, err
		}
		*field.target = value
	}
	c.Environment = strings.TrimSpace(getenv("ALBUM_DEPLOYMENT_ENVIRONMENT"))
	db, err := mysql.ParseDSN(c.DatabaseDSN)
	if err != nil || db.User == "" || strings.EqualFold(db.User, "root") || (c.Environment != "test" && c.Environment != "production") ||
		(c.Environment == "test" && !albumTestDatabasePattern.MatchString(db.DBName)) ||
		(c.Environment == "production" && db.DBName != "albums" && db.DBName != "albums_production") {
		return WorkerConfig{}, errors.New("invalid independent album database/environment")
	}
	expectedBucket := albumTestBucket
	if c.Environment == "production" {
		expectedBucket = albumProductionBucket
	}
	if c.COS.Bucket != expectedBucket || c.COS.Region != "ap-guangzhou" {
		return WorkerConfig{}, errors.New("album COS bucket/region does not match deployment environment")
	}
	c.COS.SessionToken = strings.TrimSpace(getenv("ALBUM_COS_ARCHIVE_SESSION_TOKEN"))
	c.COS.UploadSessionToken = strings.TrimSpace(getenv("ALBUM_COS_UPLOAD_SESSION_TOKEN"))
	for _, field := range []struct {
		key    string
		target *int64
	}{
		{"ALBUM_MAX_ORIGINAL_BYTES", &c.Policy.MaxOriginalBytes},
		{"ALBUM_MAX_DERIVED_BYTES", &c.Policy.MaxDerivedBytes},
	} {
		value, err := strconv.ParseInt(strings.TrimSpace(getenv(field.key)), 10, 64)
		if err != nil || value <= 0 {
			return WorkerConfig{}, errors.New("invalid " + field.key)
		}
		*field.target = value
	}
	switch strings.TrimSpace(getenv("ALBUM_PHOTO_PROCESSING_ENABLED")) {
	case "", "false":
		c.PhotoProcessing = false
	case "true":
		c.PhotoProcessing = true
	default:
		return WorkerConfig{}, errors.New("invalid ALBUM_PHOTO_PROCESSING_ENABLED")
	}
	switch strings.TrimSpace(getenv("ALBUM_VIDEO_PROCESSING_ENABLED")) {
	case "", "false":
	case "true":
		c.VideoProcessing = true
		c.VideoQueueID = strings.TrimSpace(getenv("ALBUM_CI_VIDEO_QUEUE_ID"))
		if !ciQueuePattern.MatchString(c.VideoQueueID) ||
			(c.Environment == "test" && c.VideoQueueID != albumTestVideoQueue) ||
			(c.Environment == "production" && c.VideoQueueID == albumTestVideoQueue) {
			return WorkerConfig{}, errors.New("invalid ALBUM_CI_VIDEO_QUEUE_ID")
		}
	default:
		return WorkerConfig{}, errors.New("invalid ALBUM_VIDEO_PROCESSING_ENABLED")
	}
	switch strings.TrimSpace(getenv("ALBUM_DERIVED_CLEANUP_ENABLED")) {
	case "", "false":
	case "true":
		c.DerivedCleanup = true
	default:
		return WorkerConfig{}, errors.New("invalid ALBUM_DERIVED_CLEANUP_ENABLED")
	}
	switch strings.TrimSpace(getenv("ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED")) {
	case "", "false":
	case "true":
		c.AccountMediaErasure = true
	default:
		return WorkerConfig{}, errors.New("invalid ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED")
	}
	c.Policy.UploadTTL = 24 * time.Hour
	c.Policy.VerificationLease = 30 * time.Minute
	c.BatchSize = 10
	c.PollInterval = 5 * time.Second
	if _, err := NewCOSObjects(c.COS); err != nil {
		return WorkerConfig{}, err
	}
	// Validate overflow and bounds using the same policy as the upload API.
	if c.Policy.MaxOriginalBytes > math.MaxInt64-c.Policy.MaxDerivedBytes {
		return WorkerConfig{}, errors.New("album byte limits overflow")
	}
	c.DevelopmentCloud, err = loadDevelopmentCloudConfig(getenv)
	if err != nil {
		return WorkerConfig{}, err
	}
	return c, nil
}
