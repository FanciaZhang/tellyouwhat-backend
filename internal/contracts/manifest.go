package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type Manifest struct {
	entries map[manifestKey]ManifestEntry
}

type ManifestDocument struct {
	Entries []ManifestEntry `json:"entries"`
}

type ManifestEntry struct {
	Operation               Operation `json:"operation"`
	ContractVersion         string    `json:"contractVersion"`
	PromptVersion           string    `json:"promptVersion"`
	SchemaPolicy            string    `json:"schemaPolicy"`
	SchemaSHA256            []string  `json:"schemaSHA256"`
	MaxTemperature          float64   `json:"maxTemperature"`
	AllowedReasoningEfforts []string  `json:"allowedReasoningEfforts"`
	AllowStream             bool      `json:"allowStream"`
	AllowWebSearch          bool      `json:"allowWebSearch"`
}

type manifestKey struct {
	operation       Operation
	contractVersion string
	promptVersion   string
}

func LoadManifest(path string) (*Manifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema manifest: %w", err)
	}
	return ParseManifest(content)
}

func ParseManifest(content []byte) (*Manifest, error) {
	var document ManifestDocument
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode schema manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	manifest := &Manifest{entries: make(map[manifestKey]ManifestEntry)}
	for _, entry := range document.Entries {
		key := manifestKey{entry.Operation, entry.ContractVersion, entry.PromptVersion}
		if err := validateManifestEntry(entry); err != nil {
			return nil, err
		}
		if _, exists := manifest.entries[key]; exists {
			return nil, fmt.Errorf("duplicate schema manifest entry for %s/%s", entry.Operation, entry.PromptVersion)
		}
		manifest.entries[key] = entry
	}
	for _, operation := range OperationValues() {
		policy, _ := PolicyFor(operation)
		for _, version := range []string{policy.Contract.Current, policy.Contract.Previous} {
			key := manifestKey{operation, ContractVersionV1, version}
			if _, exists := manifest.entries[key]; !exists {
				return nil, fmt.Errorf("schema manifest missing %s/%s", operation, version)
			}
		}
	}
	return manifest, nil
}

func (manifest *Manifest) Validate(request Request) error {
	if manifest == nil {
		return errors.New("schema manifest unavailable")
	}
	entry, exists := manifest.entries[manifestKey{request.Operation, request.ContractVersion, request.PromptVersion}]
	if !exists {
		return ErrUpgradeRequired
	}
	schemaPolicy := entry.SchemaPolicy
	if schemaPolicy == "" {
		schemaPolicy = "exact"
	}
	if schemaPolicy == "structured" {
		if err := ValidateStructuredMealSchema(request.ResponseSchema, request.Operation); err != nil {
			return fmt.Errorf("%w: %v", ErrContractViolation, err)
		}
	} else {
		digest, err := CanonicalJSONSHA256(request.ResponseSchema)
		if err != nil || !contains(entry.SchemaSHA256, digest) {
			return fmt.Errorf("%w: response schema is not registered", ErrContractViolation)
		}
	}
	if request.Options.TemperatureValue() > entry.MaxTemperature {
		return fmt.Errorf("%w: temperature policy", ErrContractViolation)
	}
	if request.Options.Stream && !entry.AllowStream {
		return fmt.Errorf("%w: streaming policy", ErrContractViolation)
	}
	if request.Options.WebSearchEnabled && !entry.AllowWebSearch {
		return fmt.Errorf("%w: web search policy", ErrContractViolation)
	}
	if !contains(entry.AllowedReasoningEfforts, request.Options.ReasoningEffort) {
		return fmt.Errorf("%w: reasoning policy", ErrContractViolation)
	}
	return nil
}

func CanonicalJSONSHA256(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func validateManifestEntry(entry ManifestEntry) error {
	policy, supported := PolicyFor(entry.Operation)
	if !supported || entry.ContractVersion != ContractVersionV1 ||
		(entry.PromptVersion != policy.Contract.Current && entry.PromptVersion != policy.Contract.Previous) {
		return errors.New("schema manifest contains an unsupported contract")
	}
	schemaPolicy := entry.SchemaPolicy
	if schemaPolicy == "" {
		schemaPolicy = "exact"
	}
	switch schemaPolicy {
	case "exact":
		if len(entry.SchemaSHA256) == 0 {
			return errors.New("schema manifest exact entry has no schema digest")
		}
	case "structured":
		if len(entry.SchemaSHA256) != 0 ||
			(entry.Operation != OperationMealPhotoCapture && entry.Operation != OperationMealTextCapture) {
			return errors.New("schema manifest structured entry is invalid")
		}
	default:
		return errors.New("schema manifest contains an unknown schema policy")
	}
	if entry.MaxTemperature < 0 || entry.MaxTemperature > 2 || len(entry.AllowedReasoningEfforts) == 0 {
		return errors.New("schema manifest contains an invalid policy")
	}
	for _, digest := range entry.SchemaSHA256 {
		if !sha256Pattern.MatchString(digest) {
			return errors.New("schema manifest contains an invalid digest")
		}
	}
	for _, effort := range entry.AllowedReasoningEfforts {
		switch effort {
		case "", "minimal", "low", "medium", "high", "max":
		default:
			return errors.New("schema manifest contains an invalid reasoning effort")
		}
	}
	if entry.AllowWebSearch && !policy.AllowsWebSearch {
		return errors.New("schema manifest enables unsupported web search")
	}
	return nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
