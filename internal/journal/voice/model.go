package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

type RewriteResult struct {
	Revision                  Revision
	InputTokens, OutputTokens int
	Model                     string `json:"model"`
	ConfigVersion             string `json:"configVersion"`
}
type Rewriter interface {
	Rewrite(context.Context, Snapshot, int) (RewriteResult, error)
}
type ArkRewriter struct {
	BaseURL, APIKey, Model string
	HTTP                   *http.Client
}

const rewriteInstructions = promptconfig.VoicePrompt

// Split at the manual-edit boundaries once. The model receives explicit speech
// order without counting Unicode offsets or duplicating the full transcript for
// each edit. The transport snapshot still carries its ordinary transcript string.
type transcriptSection struct {
	Start int    `json:"start"`
	Text  string `json:"text"`
}
type editorialManualEdit struct {
	ManualEdit
	HasLaterSpeech bool `json:"hasLaterSpeech"`
}
type rewriteDocument struct {
	Snapshot
	Transcript  []transcriptSection   `json:"transcript"`
	ManualEdits []editorialManualEdit `json:"manualEdits"`
}

func editorialDocument(s Snapshot) rewriteDocument {
	text := []rune(s.Transcript)
	// A late transcription is still earlier speech. Until its final receipt
	// arrives, none of this in-flight transcript can supersede that manual edit.
	s.ManualEdits = slices.Clone(s.ManualEdits)
	for i := range s.ManualEdits {
		if s.ManualEdits[i].PendingEarlierSpeech {
			s.ManualEdits[i].TranscriptOffset = len(text)
		}
	}
	boundaries := []int{0, len(text)}
	for _, edit := range s.ManualEdits {
		boundaries = append(boundaries, edit.TranscriptOffset)
	}
	slices.Sort(boundaries)
	boundaries = slices.Compact(boundaries)
	result := rewriteDocument{Snapshot: s, Transcript: []transcriptSection{}, ManualEdits: []editorialManualEdit{}}
	for _, edit := range s.ManualEdits {
		result.ManualEdits = append(result.ManualEdits, editorialManualEdit{ManualEdit: edit, HasLaterSpeech: edit.TranscriptOffset < len(text)})
	}
	for i := 1; i < len(boundaries); i++ {
		start, end := boundaries[i-1], boundaries[i]
		result.Transcript = append(result.Transcript, transcriptSection{Start: start, Text: string(text[start:end])})
	}
	return result
}

func (m ArkRewriter) Rewrite(ctx context.Context, s Snapshot, tr int) (RewriteResult, error) {
	prepared, err := PrepareRewrite(ctx, s, tr, m.Model)
	if err != nil {
		return RewriteResult{}, err
	}
	payload := prepared.Body
	ctx, cancel := context.WithTimeout(ctx, time.Duration(prepared.Parameters.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(m.BaseURL, "/")+"/responses", bytes.NewReader(payload))
	if err != nil {
		return RewriteResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return RewriteResult{}, errors.New("voice_rewrite_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return RewriteResult{}, errors.New("voice_rewrite_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return RewriteResult{}, ErrInvalid
	}
	var envelope struct {
		Status string
		Model  string
		Output []struct{ Content []struct{ Type, Text string } }
		Usage  struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		}
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return RewriteResult{}, ErrInvalid
	}
	metered := RewriteResult{Model: envelope.Model, ConfigVersion: prepared.Version, InputTokens: envelope.Usage.Input, OutputTokens: envelope.Usage.Output}
	if envelope.Usage.Input < 0 || envelope.Usage.Output < 0 {
		return metered, ErrInvalid
	}
	if envelope.Status != "completed" {
		return metered, ErrInvalid
	}
	var text string
	for _, o := range envelope.Output {
		for _, c := range o.Content {
			if c.Type == "refusal" {
				return metered, errors.New("voice_rewrite_refused")
			}
			if c.Type == "output_text" {
				text += c.Text
			}
		}
	}
	var revision Revision
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&revision); err != nil {
		return metered, ErrInvalid
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return metered, ErrInvalid
	}
	if _, err = ApplyRevision(s, revision); err != nil {
		return metered, err
	}
	if revision.TranscriptRevision != tr {
		return metered, ErrConflict
	}
	metered.Revision = revision
	return metered, nil
}

type PreparedRewrite struct {
	Body       json.RawMessage         `json:"body"`
	Parameters promptconfig.Parameters `json:"parameters"`
	Version    string                  `json:"version"`
}

func PrepareRewrite(ctx context.Context, s Snapshot, tr int, model string) (PreparedRewrite, error) {
	settings := promptconfig.Defaults(model, model, model, 60)["journal"].Journal
	version := "seed"
	if r, ok := promptconfig.FromContext(ctx); ok {
		settings = r.Policy.Journal
		version = r.ID
	}
	if err := s.Validate(); err != nil {
		return PreparedRewrite{}, err
	}
	style, err := settings.Style(string(s.WritingStyle))
	if err != nil {
		return PreparedRewrite{}, ErrInvalid
	}
	instructions := settings.Voice.Prompt
	if settings.Voice.RemoveRepetition {
		instructions += "\n整理规则：删除口头重复。"
	}
	instructions += "\n本次写作风格（仅作用于需要整理的部分）：" + style.Prompt
	input, _ := json.Marshal(map[string]any{"document": editorialDocument(s), "transcriptRevision": tr})
	fields := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "text", "afterID"}, "properties": map[string]any{"id": map[string]string{"type": "string"}, "text": map[string]string{"type": "string"}, "afterID": map[string]string{"type": "string"}}}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"baseRevision", "transcriptRevision", "patches", "questions"}, "properties": map[string]any{"baseRevision": map[string]string{"type": "integer"}, "transcriptRevision": map[string]string{"type": "integer"}, "patches": map[string]any{"type": "array", "items": fields}, "questions": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}}
	body := map[string]any{"store": false, "thinking": map[string]string{"type": "disabled"}, "instructions": instructions, "input": string(input), "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "journal_voice_revision", "strict": true, "schema": schema}}}
	settings.Voice.Parameters.Apply(body)
	payload, _ := json.Marshal(body)
	return PreparedRewrite{Body: payload, Parameters: settings.Voice.Parameters, Version: version}, nil
}

type ConfiguredRewriter struct {
	Next   Rewriter
	Config *promptconfig.Cache
}

func (r ConfiguredRewriter) Rewrite(ctx context.Context, s Snapshot, tr int) (RewriteResult, error) {
	frozen, err := promptconfig.Freeze(ctx, r.Config)
	if err != nil {
		return RewriteResult{}, err
	}
	return r.Next.Rewrite(frozen, s, tr)
}
