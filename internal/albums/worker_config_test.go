package albums

import (
	"strings"
	"testing"
	"time"
)

func workerEnvironment() map[string]string {
	return map[string]string{
		"ALBUM_DEPLOYMENT_ENVIRONMENT": "test",
		"ALBUM_DATABASE_DSN":           "test:private-password@tcp(localhost:3306)/albums_test",
		"ALBUM_COS_BUCKET":             albumTestBucket, "ALBUM_COS_REGION": "ap-guangzhou",
		"ALBUM_COS_ARCHIVE_SECRET_ID": "archive-id", "ALBUM_COS_ARCHIVE_SECRET_KEY": "private-archive-key",
		"ALBUM_COS_UPLOAD_SECRET_ID": "upload-id", "ALBUM_COS_UPLOAD_SECRET_KEY": "private-upload-key",
		"ALBUM_MAX_ORIGINAL_BYTES": "107374182400", "ALBUM_MAX_DERIVED_BYTES": "4294967296",
	}
}

func TestAlbumWorkerAndGatewayRejectCrossEnvironmentDatabases(t *testing.T) {
	for _, environment := range []string{"test", "production", "", "preview"} {
		for _, database := range []string{"albums_test", "albums_recovery_test", "albums", "albums_production", "health_test", "tellyouwhat", ""} {
			t.Run(environment+"/"+database, func(t *testing.T) {
				values := gatewayEnvironment(t)
				values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = environment
				if environment == "production" {
					values["ALBUM_COS_BUCKET"] = albumProductionBucket
				}
				values["ALBUM_DATABASE_DSN"] = "app:private-password@tcp(localhost:3306)/" + database
				want := (environment == "test" && (database == "albums_test" || database == "albums_recovery_test")) ||
					(environment == "production" && (database == "albums" || database == "albums_production"))
				_, workerErr := LoadWorkerConfig(func(k string) string { return values[k] })
				_, gatewayErr := LoadGatewayConfig(func(k string) string { return values[k] })
				for _, err := range []error{workerErr, gatewayErr} {
					if (err == nil) != want {
						t.Fatalf("worker/gateway database boundary: accepted=%t, want=%t", err == nil, want)
					}
					if err != nil && strings.Contains(err.Error(), "private-password") {
						t.Fatal("configuration error disclosed credentials")
					}
				}
			})
		}
	}
}

func TestAlbumWorkerConfigurationNeedsNoAISettings(t *testing.T) {
	values := workerEnvironment()
	config, err := LoadWorkerConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.PhotoProcessing || config.VideoProcessing || config.DerivedCleanup || config.AccountMediaErasure || config.Policy.MaxOriginalBytes != 107374182400 || config.Policy.MaxDerivedBytes != 4294967296 || config.Policy.VerificationLease != 30*time.Minute || config.COS.UploadSecretID == config.COS.SecretID {
		t.Fatal("invalid worker settings")
	}
}

func TestVideoProcessingRequiresExplicitEnableAndQueue(t *testing.T) {
	values := workerEnvironment()
	values["ALBUM_VIDEO_PROCESSING_ENABLED"] = "true"
	if _, err := LoadWorkerConfig(func(key string) string { return values[key] }); err == nil {
		t.Fatal("enabled without a private CI queue")
	}
	values["ALBUM_CI_VIDEO_QUEUE_ID"] = albumTestVideoQueue
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
		{"ALBUM_DATABASE_DSN", "root:private-password@tcp(localhost:3306)/albums_test"},
		{"ALBUM_DATABASE_DSN", "ROOT:private-password@tcp(localhost:3306)/albums_test"},
		{"ALBUM_DATABASE_DSN", "tcp(localhost:3306)/albums_test"},
		{"ALBUM_DATABASE_DSN", "not-a-dsn-private-password"},
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

func TestAccountMediaErasureRequiresExplicitEnable(t *testing.T) {
	values := workerEnvironment()
	for _, value := range []string{"", "false", "true", "1", "yes", "True"} {
		values["ALBUM_ACCOUNT_MEDIA_ERASURE_ENABLED"] = value
		config, err := LoadWorkerConfig(func(key string) string { return values[key] })
		if value != "" && value != "false" && value != "true" {
			if err == nil {
				t.Fatal("invalid permanent erasure toggle accepted")
			}
			continue
		}
		if err != nil || config.AccountMediaErasure != (value == "true") || config.DerivedCleanup {
			t.Fatal(config.AccountMediaErasure, err)
		}
	}
}

func TestAlbumWorkerAndGatewayRejectCrossEnvironmentMedia(t *testing.T) {
	for _, environment := range []string{"test", "production"} {
		for _, bucket := range []string{albumTestBucket, albumProductionBucket, "other-1251995834"} {
			for _, region := range []string{"ap-guangzhou", "ap-beijing"} {
				t.Run(environment+"/"+bucket+"/"+region, func(t *testing.T) {
					values := gatewayEnvironment(t)
					values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = environment
					if environment == "production" {
						values["ALBUM_DATABASE_DSN"] = "app:private-password@tcp(localhost:3306)/albums_production"
					}
					values["ALBUM_COS_BUCKET"], values["ALBUM_COS_REGION"] = bucket, region
					want := region == "ap-guangzhou" && ((environment == "test" && bucket == albumTestBucket) || (environment == "production" && bucket == albumProductionBucket))
					_, workerErr := LoadWorkerConfig(func(k string) string { return values[k] })
					_, gatewayErr := LoadGatewayConfig(func(k string) string { return values[k] })
					for _, err := range []error{workerErr, gatewayErr} {
						if (err == nil) != want {
							t.Fatalf("media boundary accepted=%t, want=%t", err == nil, want)
						}
					}
				})
			}
		}
	}
}

func TestAlbumVideoQueueCannotCrossDeployment(t *testing.T) {
	for _, environment := range []string{"test", "production"} {
		for _, queue := range []string{albumTestVideoQueue, "p" + strings.Repeat("b", 32)} {
			values := gatewayEnvironment(t)
			values["ALBUM_DEPLOYMENT_ENVIRONMENT"] = environment
			if environment == "production" {
				values["ALBUM_DATABASE_DSN"] = "app:synthetic@tcp(localhost:3306)/albums_production"
				values["ALBUM_COS_BUCKET"] = albumProductionBucket
			}
			values["ALBUM_VIDEO_PROCESSING_ENABLED"], values["ALBUM_CI_VIDEO_QUEUE_ID"] = "true", queue
			want := (environment == "test") == (queue == albumTestVideoQueue)
			_, workerErr := LoadWorkerConfig(func(k string) string { return values[k] })
			_, gatewayErr := LoadGatewayConfig(func(k string) string { return values[k] })
			for _, err := range []error{workerErr, gatewayErr} {
				if (err == nil) != want {
					t.Fatalf("queue boundary accepted=%t, want=%t", err == nil, want)
				}
			}
		}
	}
}
