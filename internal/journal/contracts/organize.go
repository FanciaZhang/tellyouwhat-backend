package contracts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	platformcontracts "github.com/tellyouwhat/backend/internal/contracts"
)

const (
	ContractVersion         = "journal-organize-v1"
	MaxBodyBytes            = 1 << 20
	MaxTitleRunes           = 120
	MaxBodyRunes            = 60_000
	MaxTags                 = 512
	MaxTagRunes             = 40
	MaxBooks                = 128
	MaxBookNameRunes        = 120
	MaxBookDescriptionRunes = 600
	MaxReasonRunes          = 240
	MaxRelatedTags          = 8
	MaxAnalysisVersionRunes = 128
)

var contentHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type OrganizeRequest struct {
	RequestID        string        `json:"requestID"`
	ContractVersion  string        `json:"contractVersion"`
	ContentHash      string        `json:"contentHash"`
	Title            string        `json:"title"`
	Body             string        `json:"body"`
	ExistingTags     []string      `json:"existingTags"`
	RejectedTagNames []string      `json:"rejectedTagNames"`
	Books            []BookContext `json:"books"`
}

type BookContext struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContainsEntry bool   `json:"containsEntry"`
}

type Tag struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type ExistingBookRecommendation struct {
	BookID string `json:"bookID"`
	Reason string `json:"reason"`
}

type NewBookSuggestion struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Reason      string   `json:"reason"`
	RelatedTags []string `json:"relatedTags"`
}

type Quota struct {
	DailyTokensRemaining   int  `json:"dailyTokensRemaining"`
	MonthlyTokensRemaining int  `json:"monthlyTokensRemaining"`
	Available              bool `json:"available"`
}

type OrganizeResponse struct {
	RequestID                   string                       `json:"requestID"`
	ContentHash                 string                       `json:"contentHash"`
	AnalysisVersion             string                       `json:"analysisVersion"`
	Tags                        []Tag                        `json:"tags"`
	ExistingBookRecommendations []ExistingBookRecommendation `json:"existingBookRecommendations"`
	NewBookSuggestions          []NewBookSuggestion          `json:"newBookSuggestions"`
	Quota                       Quota                        `json:"quota"`
}

var tagTypes = map[string]bool{
	"person": true, "place": true, "organization": true, "event": true,
	"topic": true, "mood": true, "other": true,
}

func (r OrganizeRequest) Validate() error {
	if !platformcontracts.ValidRequestID(r.RequestID) || !contentHashPattern.MatchString(r.ContentHash) {
		return errors.New("requestID or contentHash is invalid")
	}
	if r.ContractVersion != ContractVersion {
		return errors.New("unsupported contractVersion")
	}
	if utf8.RuneCountInString(r.Title) > MaxTitleRunes {
		return errors.New("title exceeds 120 characters")
	}
	if utf8.RuneCountInString(r.Body) > MaxBodyRunes {
		return errors.New("body exceeds 60000 characters")
	}
	if len(r.ExistingTags) > MaxTags || len(r.RejectedTagNames) > MaxTags {
		return errors.New("too many tags")
	}
	if len(r.Books) > MaxBooks {
		return errors.New("too many books")
	}
	if strings.TrimSpace(r.Title) == "" && strings.TrimSpace(r.Body) == "" {
		return errors.New("journal content is empty")
	}
	if err := validateTagNames(r.ExistingTags); err != nil {
		return fmt.Errorf("invalid existing tags: %w", err)
	}
	if err := validateTagNames(r.RejectedTagNames); err != nil {
		return fmt.Errorf("invalid rejected tags: %w", err)
	}
	seen := map[string]bool{}
	for _, b := range r.Books {
		if !platformcontracts.ValidRequestID(b.ID) || b.Name == "" || canonicalName(b.Name) != b.Name {
			return errors.New("book id and name are required")
		}
		if utf8.RuneCountInString(b.Name) > MaxBookNameRunes || utf8.RuneCountInString(b.Description) > MaxBookDescriptionRunes {
			return errors.New("book context exceeds text limits")
		}
		if seen[b.ID] {
			return errors.New("duplicate book id")
		}
		seen[b.ID] = true
	}
	return nil
}

func (r OrganizeResponse) Validate(bookIDs map[string]bool) error {
	if !platformcontracts.ValidRequestID(r.RequestID) || !contentHashPattern.MatchString(r.ContentHash) {
		return errors.New("requestID or contentHash is invalid")
	}
	if r.AnalysisVersion == "" || strings.TrimSpace(r.AnalysisVersion) != r.AnalysisVersion || utf8.RuneCountInString(r.AnalysisVersion) > MaxAnalysisVersionRunes {
		return errors.New("analysisVersion is invalid")
	}
	if r.Quota.DailyTokensRemaining < 0 || r.Quota.MonthlyTokensRemaining < 0 {
		return errors.New("quota is invalid")
	}
	return (ModelResult{
		Tags:                        r.Tags,
		ExistingBookRecommendations: r.ExistingBookRecommendations,
		NewBookSuggestions:          r.NewBookSuggestions,
	}).Validate(bookIDs)
}

func (r ModelResult) Validate(bookIDs map[string]bool) error {
	if len(r.Tags) > 8 || len(r.ExistingBookRecommendations) > 3 || len(r.NewBookSuggestions) > 2 {
		return errors.New("model result exceeds item limits")
	}
	seenTags := map[string]bool{}
	for _, tag := range r.Tags {
		n := normalizedName(tag.Name)
		if n == "" || canonicalName(tag.Name) != tag.Name || utf8.RuneCountInString(tag.Name) > MaxTagRunes || !tagTypes[tag.Type] {
			return errors.New("invalid tag")
		}
		if seenTags[n] {
			return errors.New("duplicate tag")
		}
		seenTags[n] = true
	}
	seenBookIDs := map[string]bool{}
	for _, rec := range r.ExistingBookRecommendations {
		if !bookIDs[rec.BookID] || seenBookIDs[rec.BookID] || !validExplanation(rec.Reason) {
			return fmt.Errorf("invalid book recommendation %q", rec.BookID)
		}
		seenBookIDs[rec.BookID] = true
	}
	seenSuggestions := map[string]bool{}
	for _, suggestion := range r.NewBookSuggestions {
		name := normalizedName(suggestion.Name)
		if name == "" || canonicalName(suggestion.Name) != suggestion.Name || utf8.RuneCountInString(suggestion.Name) > MaxBookNameRunes ||
			utf8.RuneCountInString(suggestion.Description) > MaxBookDescriptionRunes || !validExplanation(suggestion.Reason) ||
			seenSuggestions[name] || len(suggestion.RelatedTags) > MaxRelatedTags {
			return errors.New("invalid new book suggestion")
		}
		seenSuggestions[name] = true
		if err := validateTagNames(suggestion.RelatedTags); err != nil {
			return errors.New("invalid new book related tags")
		}
	}
	return nil
}

func canonicalName(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizedName(value string) string {
	return strings.ToLower(canonicalName(value))
}

func validateTagNames(values []string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		normalized := normalizedName(value)
		if normalized == "" || canonicalName(value) != value || utf8.RuneCountInString(value) > MaxTagRunes || seen[normalized] {
			return errors.New("tag name is empty, duplicated, or exceeds limits")
		}
		seen[normalized] = true
	}
	return nil
}

func validExplanation(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && utf8.RuneCountInString(value) <= MaxReasonRunes
}

type ModelResult struct {
	Tags                        []Tag                        `json:"tags"`
	ExistingBookRecommendations []ExistingBookRecommendation `json:"existingBookRecommendations"`
	NewBookSuggestions          []NewBookSuggestion          `json:"newBookSuggestions"`
}

func ResponseSchema() map[string]any {
	str := func(maxLength int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": maxLength}
	}
	tagItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"name", "type"},
		"properties": map[string]any{
			"name": str(MaxTagRunes),
			"type": map[string]any{"type": "string", "enum": []string{"person", "place", "organization", "event", "topic", "mood", "other"}},
		},
	}
	bookItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"bookID", "reason"},
		"properties":           map[string]any{"bookID": str(16), "reason": str(MaxReasonRunes)},
	}
	newBookItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"name", "description", "reason", "relatedTags"},
		"properties": map[string]any{
			"name":        str(MaxBookNameRunes),
			"description": map[string]any{"type": "string", "maxLength": MaxBookDescriptionRunes},
			"reason":      str(MaxReasonRunes),
			"relatedTags": map[string]any{"type": "array", "maxItems": MaxRelatedTags, "items": str(MaxTagRunes)},
		},
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"tags", "existingBookRecommendations", "newBookSuggestions"},
		"properties": map[string]any{
			"tags":                        map[string]any{"type": "array", "maxItems": 8, "items": tagItem},
			"existingBookRecommendations": map[string]any{"type": "array", "maxItems": 3, "items": bookItem},
			"newBookSuggestions":          map[string]any{"type": "array", "maxItems": 2, "items": newBookItem},
		},
	}
}
