package contracts

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	ContractVersion = "journal-organize-v1"
	MaxBodyBytes    = 1 << 20
	MaxBodyRunes    = 60_000
	MaxTags         = 512
	MaxBooks        = 128
)

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
	DailyRemaining   int `json:"dailyRemaining"`
	MonthlyRemaining int `json:"monthlyRemaining"`
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
	if r.RequestID == "" || r.ContentHash == "" {
		return errors.New("requestID and contentHash are required")
	}
	if r.ContractVersion != ContractVersion {
		return errors.New("unsupported contractVersion")
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
	seen := map[string]bool{}
	for _, b := range r.Books {
		if b.ID == "" || b.Name == "" {
			return errors.New("book id and name are required")
		}
		if seen[b.ID] {
			return errors.New("duplicate book id")
		}
		seen[b.ID] = true
	}
	return nil
}

func (r OrganizeResponse) Validate(bookIDs map[string]bool) error {
	if len(r.Tags) > 8 || len(r.ExistingBookRecommendations) > 3 || len(r.NewBookSuggestions) > 2 {
		return errors.New("model result exceeds item limits")
	}
	seenTags := map[string]bool{}
	for _, tag := range r.Tags {
		n := strings.ToLower(strings.TrimSpace(tag.Name))
		if n == "" || utf8.RuneCountInString(tag.Name) > 40 || !tagTypes[tag.Type] {
			return errors.New("invalid tag")
		}
		if seenTags[n] {
			return errors.New("duplicate tag")
		}
		seenTags[n] = true
	}
	for _, rec := range r.ExistingBookRecommendations {
		if !bookIDs[rec.BookID] || strings.TrimSpace(rec.Reason) == "" {
			return fmt.Errorf("invalid book recommendation %q", rec.BookID)
		}
	}
	for _, suggestion := range r.NewBookSuggestions {
		if strings.TrimSpace(suggestion.Name) == "" || strings.TrimSpace(suggestion.Reason) == "" {
			return errors.New("invalid new book suggestion")
		}
	}
	return nil
}

type ModelResult struct {
	Tags                        []Tag                        `json:"tags"`
	ExistingBookRecommendations []ExistingBookRecommendation `json:"existingBookRecommendations"`
	NewBookSuggestions          []NewBookSuggestion          `json:"newBookSuggestions"`
}

func ResponseSchema() map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	tagItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"name", "type"},
		"properties": map[string]any{
			"name": str(),
			"type": map[string]any{"type": "string", "enum": []string{"person", "place", "organization", "event", "topic", "mood", "other"}},
		},
	}
	bookItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"bookID", "reason"},
		"properties":           map[string]any{"bookID": str(), "reason": str()},
	}
	newBookItem := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"name", "description", "reason", "relatedTags"},
		"properties": map[string]any{
			"name": str(), "description": str(), "reason": str(),
			"relatedTags": map[string]any{"type": "array", "items": str()},
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
