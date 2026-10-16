package config

import "testing"

func TestJournalImagesRequireExplicitProviderAndBoundedPrice(t *testing.T) {
	t.Setenv("PLATFORM_APPS", "journal")
	t.Setenv("JOURNAL_IMAGE_ENABLED", "true")
	t.Setenv("JOURNAL_IMAGE_BASE_URL", "https://image.example/api/v3")
	t.Setenv("JOURNAL_IMAGE_API_KEY", "synthetic")
	t.Setenv("JOURNAL_IMAGE_MODEL_ID", "image-model")
	t.Setenv("JOURNAL_IMAGE_MAX_COST_NANOS", "300000000")
	cfg, err := loadPlatformUnchecked()
	if err != nil || cfg.Apps[0].JournalImage == nil {
		t.Fatal("valid image configuration rejected", err)
	}
	for key, value := range map[string]string{"JOURNAL_IMAGE_ENABLED": "yes", "JOURNAL_IMAGE_BASE_URL": "http://image.example", "JOURNAL_IMAGE_API_KEY": "", "JOURNAL_IMAGE_MODEL_ID": "", "JOURNAL_IMAGE_MAX_COST_NANOS": "1000000001"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := loadPlatformUnchecked(); err == nil {
				t.Fatal("invalid image configuration accepted")
			}
		})
	}
	t.Setenv("JOURNAL_IMAGE_ENABLED", "false")
	t.Setenv("JOURNAL_IMAGE_MAX_COST_NANOS", "invalid-unused")
	cfg, err = loadPlatformUnchecked()
	if err != nil || cfg.Apps[0].JournalImage != nil {
		t.Fatal("disabled image feature loaded credentials", err)
	}
}
