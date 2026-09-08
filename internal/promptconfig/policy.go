// Package promptconfig owns trusted, versioned prompts and runtime parameters.
package promptconfig

import (
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid prompt configuration")
	ErrConflict = errors.New("prompt configuration version conflict")
	ErrNotFound = errors.New("prompt configuration not found")
	ErrNotReady = errors.New("prompt configuration not loaded")
)
var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type Parameters struct {
	FoundationModel string                  `json:"foundationModel,omitempty"`
	ModelVersion    string                  `json:"modelVersion,omitempty"`
	Price           *costcontrol.TokenPrice `json:"price,omitempty"`
	Model           string                  `json:"model"`
	ReasoningEffort string                  `json:"reasoningEffort"`
	Temperature     *float64                `json:"temperature"`
	MaxOutputTokens int                     `json:"maxOutputTokens"`
	TimeoutSeconds  int                     `json:"timeoutSeconds"`
}

func (p Parameters) Validate() error {
	if p.Price != nil && !p.Price.Valid() {
		return ErrInvalid
	}
	if !identifier.MatchString(p.Model) || p.MaxOutputTokens < 256 || p.MaxOutputTokens > 65536 || p.TimeoutSeconds < 5 || p.TimeoutSeconds > 840 {
		return ErrInvalid
	}
	switch p.ReasoningEffort {
	case "disabled", "minimal", "low", "medium", "high":
	default:
		return ErrInvalid
	}
	if p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
		return ErrInvalid
	}
	return nil
}

// Apply keeps structural output contracts and provider credentials out of configuration.
func (p Parameters) Apply(body map[string]any) {
	body["model"], body["max_output_tokens"] = p.Model, p.MaxOutputTokens
	if p.ReasoningEffort == "disabled" {
		body["thinking"] = map[string]string{"type": "disabled"}
	} else {
		body["thinking"] = map[string]string{"type": "enabled"}
		body["reasoning"] = map[string]string{"effort": p.ReasoningEffort}
	}
	if p.Temperature != nil {
		body["temperature"] = *p.Temperature
	}
}

type Organize struct {
	Prompt            string     `json:"prompt"`
	Lite              Parameters `json:"lite"`
	Pro               Parameters `json:"pro"`
	LiteMaxCharacters int        `json:"liteMaxCharacters"`
	LiteMaxBooks      int        `json:"liteMaxBooks"`
	LiteMaxTags       int        `json:"liteMaxTags"`
}
type Voice struct {
	Prompt               string     `json:"prompt"`
	RemoveRepetition     bool       `json:"removeRepetition"`
	Parameters           Parameters `json:"parameters"`
	AutomaticPunctuation bool       `json:"automaticPunctuation"`
	NormalizeNumbers     bool       `json:"normalizeNumbers"`
}
type Style struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Order       int    `json:"order"`
	Enabled     bool   `json:"enabled"`
	Prompt      string `json:"prompt"`
}
type Journal struct {
	Organize     Organize `json:"organize"`
	Voice        Voice    `json:"voice"`
	DefaultStyle string   `json:"defaultStyle"`
	Styles       []Style  `json:"styles"`
}
type Policy struct {
	SystemPrompt string   `json:"systemPrompt"`
	Journal      *Journal `json:"journal,omitempty"`
}
type Revision struct {
	ID          string     `json:"id"`
	Scope       string     `json:"scope"`
	BaseVersion string     `json:"baseVersion"`
	Policy      Policy     `json:"policy"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
}

func Scopes() []string {
	out := []string{"journal"}
	for _, op := range contracts.OperationValues() {
		out = append(out, "health:"+string(op))
	}
	return out
}
func validText(s string, limit int, required bool) bool {
	return len(s) <= limit && (!required || strings.TrimSpace(s) != "")
}
func (p Policy) Validate(scope string) error {
	valid := false
	for _, v := range Scopes() {
		valid = valid || v == scope
	}
	if !valid {
		return ErrInvalid
	}
	if scope != "journal" {
		if p.Journal != nil || !validText(p.SystemPrompt, 64000, false) {
			return ErrInvalid
		}
		return nil
	}
	j := p.Journal
	if j == nil || p.SystemPrompt != "" || !validText(j.Organize.Prompt, 64000, true) || !validText(j.Voice.Prompt, 64000, true) || j.Organize.Lite.Validate() != nil || j.Organize.Pro.Validate() != nil || j.Voice.Parameters.Validate() != nil {
		return ErrInvalid
	}
	if j.Organize.LiteMaxCharacters < 1 || j.Organize.LiteMaxCharacters > 60000 || j.Organize.LiteMaxBooks < 0 || j.Organize.LiteMaxBooks > 1000 || j.Organize.LiteMaxTags < 0 || j.Organize.LiteMaxTags > 1000 || len(j.Styles) < 1 || len(j.Styles) > 100 {
		return ErrInvalid
	}
	ids := map[string]bool{}
	def := false
	for _, s := range j.Styles {
		if !identifier.MatchString(s.ID) || ids[s.ID] || !validText(s.Name, 200, true) || !validText(s.Description, 2000, false) || !validText(s.Example, 8000, false) || !validText(s.Prompt, 16000, true) || s.Order < 0 || s.Order > 10000 {
			return ErrInvalid
		}
		ids[s.ID] = true
		def = def || (s.ID == j.DefaultStyle && s.Enabled)
	}
	if !def {
		return ErrInvalid
	}
	return nil
}
func (j Journal) Style(id string) (Style, error) {
	if id == "" {
		id = j.DefaultStyle
	}
	for _, s := range j.Styles {
		if s.ID == id {
			return s, nil
		}
	}
	return Style{}, ErrNotFound
}
func clone[T any](v T) T {
	raw, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(raw, &out)
	return out
}
