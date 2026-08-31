package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/provider/ark"
)

type AppStoreConfig struct {
	Environment    string
	IssuerID       string
	KeyID          string
	PrivateKeyPath string
	RootPEMPath    string
	AppAppleID     int64
}

func (config AppStoreConfig) validate() error {
	if config.Environment != "Production" && config.Environment != "Sandbox" && config.Environment != "Both" {
		return errors.New("APP_STORE_ENV must be Production, Sandbox, or Both")
	}
	if config.IssuerID == "" || config.KeyID == "" || config.PrivateKeyPath == "" || config.RootPEMPath == "" {
		return errors.New("APP_STORE_ISSUER_ID, APP_STORE_KEY_ID, APP_STORE_PRIVATE_KEY_PATH, and APP_STORE_ROOT_PEM_PATH are required")
	}
	if (config.Environment == "Production" || config.Environment == "Both") && config.AppAppleID <= 0 {
		return errors.New("APP_STORE_APP_APPLE_ID is required when Production is enabled")
	}
	return nil
}

func validateArkConfig(config ark.Config) error {
	if config.APIKey == "" {
		return errors.New("ARK_API_KEY is required")
	}
	for _, operation := range contracts.OperationValues() {
		route, ok := config.Routes[operation]
		if !ok || route.Model == "" {
			return fmt.Errorf("%s is required for Ark operation %s", arkEndpointEnvironmentKeys[operation], operation)
		}
		if route.TimeoutSeconds <= 0 || route.TimeoutSeconds > 14*60 {
			return errors.New("ARK_TIMEOUT_SECONDS must be between 1 and 840")
		}
	}
	return nil
}

var arkEndpointEnvironmentKeys = map[contracts.Operation]string{
	contracts.OperationVoiceTranscription:      "ARK_ENDPOINT_VOICE_TRANSCRIPTION",
	contracts.OperationMealPhotoCapture:        "ARK_ENDPOINT_MEAL_PHOTO_CAPTURE",
	contracts.OperationMealTextCapture:         "ARK_ENDPOINT_MEAL_TEXT_CAPTURE",
	contracts.OperationMealDecision:            "ARK_ENDPOINT_MEAL_DECISION",
	contracts.OperationDietAnalysis:            "ARK_ENDPOINT_DIET_ANALYSIS",
	contracts.OperationHealthNutritionAnalysis: "ARK_ENDPOINT_HEALTH_NUTRITION_ANALYSIS",
	contracts.OperationHealthBehaviorAnalysis:  "ARK_ENDPOINT_HEALTH_BEHAVIOR_ANALYSIS",
}

func value(key, fallback string) string {
	if result := strings.TrimSpace(os.Getenv(key)); result != "" {
		return result
	}
	return fallback
}

func splitSet(raw string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
