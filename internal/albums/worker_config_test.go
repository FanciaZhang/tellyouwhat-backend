package albums

import (
	"strings"
	"testing"
	"time"
)

func workerEnvironment() map[string]string {
	return map[string]string{
		"ALBUM_DATABASE_DSN": "test:private-password@tcp(localhost:3306)/albums_test",
		"ALBUM_COS_BUCKET":   "album-test-1250000000", "ALBUM_COS_REGION": "ap-guangzhou",
		"ALBUM_COS_ARCHIVE_SECRET_ID": "archive-id", "ALBUM_COS_ARCHIVE_SECRET_KEY": "private-archive-key",
		"ALBUM_COS_UPLOAD_SECRET_ID": "upload-id", "ALBUM_COS_UPLOAD_SECRET_KEY": "private-upload-key",
		"ALBUM_MAX_ORIGINAL_BYTES": "107374182400", "ALBUM_MAX_DERIVED_BYTES": "4294967296",
	}
}

func TestAlbumWorkerConfigurationNeedsNoAISettings(t *testing.T) {
	values := workerEnvironment()
	config, err := LoadWorkerConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.PhotoProcessing || config.VideoProcessing || config.DerivedCleanup || config.Policy.MaxOriginalBytes != 107374182400 || config.Policy.MaxDerivedBytes != 4294967296 || config.Policy.VerificationLease != 30*time.Minute || config.COS.UploadSecretID == config.COS.SecretID {
		t.Fatal("invalid worker settings")
	}
}

func TestVideoProcessingRequiresExplicitEnableAndQueue(t *testing.T) {
	values := workerEnvironment()
	values["ALBUM_VIDEO_PROCESSING_ENABLED"] = "true"
	if _, err := LoadWorkerConfig(func(key string) string { return values[key] }); err == nil {
		t.Fatal("enabled without a private CI queue")
	}
	values["ALBUM_CI_VIDEO_QUEUE_ID"] = "p" + strings.Repeat("a", 32)
	config, err := LoadWorkerConfig(func(key string) string { return values[key] })
	if err != nil || !config.VideoProcessing || config.VideoQueueID != values["ALBUM_CI_VIDEO_QUEUE_ID"] {
		t.Fatal("video processing not enabled", err)
	}
	for _, value := range []string{"yes", "1", "True"} {
		values["ALBUM_VIDEO_PROCESSING_ENABLED"] = value
		if _, err := LoadWorkerConfig(func(key string) string { return values[key] }); err == nil {
			t.Fatal("unrecognized toggle accepted")
		}
	}
}

func TestAlbumWorkerRejectsIncompleteOrUnsafeConfigurationWithoutSecrets(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"ALBUM_DATABASE_DSN", ""}, {"ALBUM_COS_UPLOAD_SECRET_KEY", ""},
		{"ALBUM_COS_UPLOAD_SECRET_ID", "archive-id"},
		{"ALBUM_COS_REGION", "https://private-token.invalid"},
		{"ALBUM_MAX_ORIGINAL_BYTES", "9223372036854775807"},
		{"ALBUM_MAX_DERIVED_BYTES", "0"},
		{"ALBUM_PHOTO_PROCESSING_ENABLED", "yes"},
		{"ALBUM_DERIVED_CLEANUP_ENABLED", "yes"},
	} {
		t.Run(test.key, func(t *testing.T) {
			values := workerEnvironment()
			values[test.key] = test.value
			_, err := LoadWorkerConfig(func(key string) string { return values[key] })
			if err == nil {
				t.Fatal("invalid worker configuration accepted")
			}
			for _, secret := range []string{"private-password", "private-archive-key", "private-upload-key", "private-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("configuration error leaked a secret")
				}
			}
		})
	}
}

func TestPhotoProcessingExplicitEnable(t *testing.T) {
	values := workerEnvironment()
	values["ALBUM_PHOTO_PROCESSING_ENABLED"] = "true"
	config, err := LoadWorkerConfig(func(key string) string { return values[key] })
	if err != nil || !config.PhotoProcessing {
		t.Fatal("photo processing not enabled", err)
	}
}

func TestDerivedCleanupRequiresExplicitEnable(t *testing.T) {
	values := workerEnvironment()
	values["ALBUM_DERIVED_CLEANUP_ENABLED"] = "true"
	config, err := LoadWorkerConfig(func(key string) string { return values[key] })
	if err != nil || !config.DerivedCleanup || config.PhotoProcessing || config.VideoProcessing {
		t.Fatal("cleanup must be independently enabled", err)
	}
	for _, value := range []string{"1", "True", "yes"} {
		values["ALBUM_DERIVED_CLEANUP_ENABLED"] = value
		if _, err := LoadWorkerConfig(func(key string) string { return values[key] }); err == nil {
			t.Fatal("invalid cleanup toggle accepted")
		}
	}
}
