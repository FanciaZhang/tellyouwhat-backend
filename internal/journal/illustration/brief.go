package illustration

import (
	"context"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"strings"
	"unicode/utf8"
)

// Brief is user material, never an application-compiled provider prompt.
// Source quotes and local document identifiers are intentionally not uploaded.
type Brief struct {
	Title   string   `json:"title"`
	Details []Detail `json:"details"`
}
type Detail struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (b Brief) Validate() error {
	if !utf8.ValidString(b.Title) || strings.TrimSpace(b.Title) == "" || utf8.RuneCountInString(b.Title) > 300 || len(b.Details) < 1 || len(b.Details) > 64 {
		return ErrInput
	}
	total := utf8.RuneCountInString(b.Title)
	for _, d := range b.Details {
		switch d.Kind {
		case "subject", "setting", "composition", "style", "other":
		default:
			return ErrInput
		}
		if !utf8.ValidString(d.Text) || strings.TrimSpace(d.Text) == "" || utf8.RuneCountInString(d.Text) > 8000 {
			return ErrInput
		}
		total += utf8.RuneCountInString(d.Text)
	}
	if total > 18000 || utf8.RuneCountInString(b.Compile(context.Background())) > 20000 {
		return ErrInput
	}
	return nil
}

// Compile runs once at admission; the encrypted job freezes the exact prompt
// across retries and process restarts without trusting instructions from an App.
func (b Brief) Compile(ctx context.Context) string {
	instructions := promptconfig.IllustrationGenerationPrompt
	if r, ok := promptconfig.FromContext(ctx); ok && r.Policy.Journal != nil {
		instructions = r.Policy.Journal.IllustrationGenerationPrompt
	}
	encoded, _ := json.Marshal(b)
	return instructions + "\n画面素材：" + string(encoded)
}
