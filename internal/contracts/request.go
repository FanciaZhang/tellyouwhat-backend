package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	ContractVersionV1 = "ai-request-v1"
	DefaultBodyLimit  = 16 << 20
)

var (
	ErrContractViolation = errors.New("business contract violation")
	ErrUpgradeRequired   = errors.New("client upgrade required")
	ErrPayloadTooLarge   = errors.New("payload too large")
)

type Operation string

const (
	OperationVoiceTranscription      Operation = "health.voice.transcription"
	OperationMealPhotoCapture        Operation = "health.meal.photo-capture"
	OperationMealTextCapture         Operation = "health.meal.text-capture"
	OperationMealDecision            Operation = "health.meal.decision"
	OperationDietAnalysis            Operation = "health.diet.analysis"
	OperationHealthNutritionAnalysis Operation = "health.nutrition.analysis"
	OperationHealthBehaviorAnalysis  Operation = "health.behavior.analysis"
)

type Request struct {
	RequestID         string          `json:"requestID"`
	Operation         Operation       `json:"operation"`
	ContractVersion   string          `json:"contractVersion"`
	PromptVersion     string          `json:"promptVersion"`
	Prompt            string          `json:"prompt"`
	ResponseSchema    json.RawMessage `json:"responseSchema"`
	Options           RequestOptions  `json:"options"`
	Media             []Media         `json:"media"`
	SemanticSignature string          `json:"semanticSignature"`
}

type RequestOptions struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	ReasoningEffort  string   `json:"reasoningEffort,omitempty"`
	WebSearchEnabled bool     `json:"webSearchEnabled,omitempty"`
}

func (options RequestOptions) TemperatureValue() float64 {
	if options.Temperature == nil {
		return 0
	}
	return *options.Temperature
}

type Media struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MIMEType  string `json:"mimeType"`
	ObjectID  string `json:"objectID"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type PromptContract struct {
	Current  string
	Previous string
}

type OperationPolicy struct {
	Contract        PromptContract
	MaxPromptBytes  int
	MaxSchemaBytes  int
	MaxMediaCount   int
	MaxMediaBytes   int64
	AllowedMedia    map[string]map[string]struct{}
	AllowsWebSearch bool
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func DecodeAndValidate(reader io.Reader, maxBodyBytes int64) (Request, error) {
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil {
		return Request{}, fmt.Errorf("%w: read request", ErrContractViolation)
	}
	if int64(len(body)) > maxBodyBytes {
		return Request{}, ErrPayloadTooLarge
	}

	var request Request
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("%w: invalid JSON: %v", ErrContractViolation, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Request{}, err
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (request Request) Validate() error {
	policy, ok := PolicyFor(request.Operation)
	if !ok {
		return fmt.Errorf("%w: unsupported operation", ErrContractViolation)
	}
	if request.ContractVersion != ContractVersionV1 {
		return ErrUpgradeRequired
	}
	if request.PromptVersion != policy.Contract.Current && request.PromptVersion != policy.Contract.Previous {
		return ErrUpgradeRequired
	}
	if !uuidPattern.MatchString(request.RequestID) {
		return fmt.Errorf("%w: invalid requestID", ErrContractViolation)
	}
	if len(request.Prompt) == 0 || len(request.Prompt) > policy.MaxPromptBytes {
		return fmt.Errorf("%w: prompt length", ErrContractViolation)
	}
	if len(request.SemanticSignature) == 0 || len(request.SemanticSignature) > 256 {
		return fmt.Errorf("%w: semantic signature", ErrContractViolation)
	}
	if request.Options.Temperature != nil && (*request.Options.Temperature < 0 || *request.Options.Temperature > 2) {
		return fmt.Errorf("%w: temperature", ErrContractViolation)
	}
	switch request.Options.ReasoningEffort {
	case "", "minimal", "low", "medium", "high", "max":
	default:
		return fmt.Errorf("%w: reasoning effort", ErrContractViolation)
	}
	if request.Options.WebSearchEnabled && !policy.AllowsWebSearch {
		return fmt.Errorf("%w: web search is not allowed for this operation", ErrContractViolation)
	}
	if err := validateResponseSchema(request.ResponseSchema, policy.MaxSchemaBytes); err != nil {
		return err
	}
	if len(request.Media) > policy.MaxMediaCount {
		return ErrPayloadTooLarge
	}
	mediaIDs := make(map[string]struct{}, len(request.Media))
	objectIDs := make(map[string]struct{}, len(request.Media))
	for _, media := range request.Media {
		if err := validateMedia(media, policy); err != nil {
			return err
		}
		if _, duplicate := mediaIDs[media.ID]; duplicate {
			return fmt.Errorf("%w: duplicate media ID", ErrContractViolation)
		}
		if _, duplicate := objectIDs[media.ObjectID]; duplicate {
			return fmt.Errorf("%w: duplicate media object", ErrContractViolation)
		}
		mediaIDs[media.ID] = struct{}{}
		objectIDs[media.ObjectID] = struct{}{}
	}
	return nil
}

func PolicyFor(operation Operation) (OperationPolicy, bool) {
	contract, ok := promptContracts[operation]
	if !ok {
		return OperationPolicy{}, false
	}
	policy := OperationPolicy{
		Contract:       contract,
		MaxPromptBytes: 512 << 10,
		MaxSchemaBytes: 128 << 10,
		MaxMediaCount:  0,
		MaxMediaBytes:  0,
		AllowedMedia:   map[string]map[string]struct{}{},
	}
	switch operation {
	case OperationVoiceTranscription:
		policy.MaxMediaCount = 1
		policy.MaxMediaBytes = 25 << 20
		policy.AllowedMedia = allowedMedia("audio", "audio/m4a", "audio/mp4", "audio/mpeg", "audio/wav")
	case OperationMealPhotoCapture:
		policy.MaxMediaCount = 4
		policy.MaxMediaBytes = 20 << 20
		policy.AllowedMedia = allowedMedia("image", "image/jpeg", "image/heic", "image/png")
	case OperationMealDecision:
		policy.MaxMediaCount = 4
		policy.MaxMediaBytes = 20 << 20
		policy.AllowedMedia = allowedMedia("image", "image/jpeg", "image/heic", "image/png")
		policy.AllowsWebSearch = true
	}
	return policy, true
}

func OperationValues() []Operation {
	return []Operation{
		OperationVoiceTranscription,
		OperationMealPhotoCapture,
		OperationMealTextCapture,
		OperationMealDecision,
		OperationDietAnalysis,
		OperationHealthNutritionAnalysis,
		OperationHealthBehaviorAnalysis,
	}
}

func ValidateMediaForOperation(operation Operation, media Media) error {
	policy, ok := PolicyFor(operation)
	if !ok {
		return fmt.Errorf("%w: unsupported operation", ErrContractViolation)
	}
	return validateMedia(media, policy)
}

func ValidRequestID(value string) bool {
	return uuidPattern.MatchString(value)
}

// ReservationTokens deliberately overestimates text and schema tokens by
// counting UTF-8 bytes one-for-one, then adds a fixed output and modality
// budget. This prevents the post-response reconciliation from being the first
// cost guard.
func ReservationTokens(request Request) int {
	reserved := len(request.Prompt) + len(request.ResponseSchema) + 4_096 + 1024
	for _, item := range request.Media {
		switch item.Kind {
		case "audio":
			reserved += 65_536
		case "image":
			reserved += 32_768
		}
	}
	return reserved
}

var promptContracts = map[Operation]PromptContract{
	OperationVoiceTranscription:      {Current: "voice-transcription-v1", Previous: "voice-transcription-v0"},
	OperationMealPhotoCapture:        {Current: "meal-photo-v5", Previous: "meal-photo-v4"},
	OperationMealTextCapture:         {Current: "meal-text-v5", Previous: "meal-text-v4"},
	OperationMealDecision:            {Current: "meal-decision-v10-fresh-exploration", Previous: "meal-decision-v9"},
	OperationDietAnalysis:            {Current: "diet-day-review-v4", Previous: "diet-day-review-v3"},
	OperationHealthNutritionAnalysis: {Current: "health-nutrition-v1", Previous: "health-nutrition-v0"},
	OperationHealthBehaviorAnalysis:  {Current: "health-behavior-v1", Previous: "health-behavior-v0"},
}

func validateResponseSchema(raw json.RawMessage, maxBytes int) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return fmt.Errorf("%w: response schema length", ErrContractViolation)
	}
	var schema struct {
		Type                 string `json:"type"`
		AdditionalProperties *bool  `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("%w: response schema JSON", ErrContractViolation)
	}
	if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		return fmt.Errorf("%w: response schema must be a strict object", ErrContractViolation)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil || containsRemoteReference(document) {
		return fmt.Errorf("%w: response schema reference", ErrContractViolation)
	}
	return nil
}

func validateMedia(media Media, policy OperationPolicy) error {
	allowedMIMEs, kindAllowed := policy.AllowedMedia[media.Kind]
	_, mimeAllowed := allowedMIMEs[media.MIMEType]
	if !kindAllowed || !mimeAllowed {
		return fmt.Errorf("%w: media type", ErrContractViolation)
	}
	if media.ID == "" || media.ObjectID == "" || !sha256Pattern.MatchString(media.SHA256) {
		return fmt.Errorf("%w: media identity", ErrContractViolation)
	}
	if media.SizeBytes <= 0 || media.SizeBytes > policy.MaxMediaBytes {
		return ErrPayloadTooLarge
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", ErrContractViolation)
	}
	return nil
}

func allowedMedia(kind string, mimeTypes ...string) map[string]map[string]struct{} {
	allowed := make(map[string]struct{}, len(mimeTypes))
	for _, mimeType := range mimeTypes {
		allowed[strings.ToLower(mimeType)] = struct{}{}
	}
	return map[string]map[string]struct{}{kind: allowed}
}
