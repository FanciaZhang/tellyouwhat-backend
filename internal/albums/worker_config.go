package albums

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// WorkerConfig is deliberately separate from the managed-AI worker settings.
// No AI product, provider key, or device identity is required for album storage.
type WorkerConfig struct {
	DatabaseDSN  string
	COS          COSConfig
	Policy       UploadPolicy
	BatchSize    int
	PollInterval time.Duration
}

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
	return c, nil
}
