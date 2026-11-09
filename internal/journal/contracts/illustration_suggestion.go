package contracts

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Optional voice advice is independently decoded to preserve a valid rewrite.
// Dedicated scene requests reject malformed or ungrounded advice as retryable errors.
type IllustrationSuggestion struct {
	Summary      string   `json:"summary"`
	Subject      string   `json:"subject"`
	Setting      string   `json:"setting"`
	Composition  string   `json:"composition"`
	Style        string   `json:"style"`
	SourceQuotes []string `json:"sourceQuotes"`
	Reason       string   `json:"reason"`
}

func (s *IllustrationSuggestion) UnmarshalJSON(data []byte) error {
	type scene IllustrationSuggestion
	var value scene
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil {
		*s = IllustrationSuggestion{}
		return nil
	}
	*s = IllustrationSuggestion(value)
	return nil
}

func (s *IllustrationSuggestion) Valid() bool {
	if s == nil || len(s.SourceQuotes) == 0 || len(s.SourceQuotes) > 3 {
		return false
	}
	for _, value := range []struct {
		text     string
		maximum  int
		required bool
	}{
		{s.Summary, 120, true}, {s.Subject, 500, true}, {s.Setting, 500, false},
		{s.Composition, 500, false}, {s.Style, 120, false}, {s.Reason, 240, true},
	} {
		if !utf8.ValidString(value.text) || utf8.RuneCountInString(value.text) > value.maximum ||
			value.required && strings.TrimSpace(value.text) == "" {
			return false
		}
	}
	seen := map[string]bool{}
	for _, quote := range s.SourceQuotes {
		if strings.TrimSpace(quote) == "" || utf8.RuneCountInString(quote) > 600 || seen[quote] {
			return false
		}
		seen[quote] = true
	}
	return true
}

// Ground quotes in material already provided for this analysis. Never expand
// the speech context window or send additional content merely to find an image.
func (s *IllustrationSuggestion) Grounded(material []string) bool {
	if !s.Valid() {
		return false
	}
	for _, quote := range s.SourceQuotes {
		found := false
		for _, text := range material {
			if strings.Contains(text, quote) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func IllustrationSuggestionSchema() map[string]any {
	str := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	return map[string]any{
		"type": []string{"object", "null"}, "additionalProperties": false,
		"required": []string{"summary", "subject", "setting", "composition", "style", "sourceQuotes", "reason"},
		"properties": map[string]any{
			"summary": str(120), "subject": str(500), "setting": str(500), "composition": str(500), "style": str(120), "reason": str(240),
			"sourceQuotes": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "items": str(600)},
		},
	}
}
