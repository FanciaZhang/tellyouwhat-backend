package readaloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrCatalog = errors.New("invalid read-aloud voice catalog")

type Voice struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description"`
	Languages     []string           `json:"languages"`
	Styles        []string           `json:"styles"`
	Dialects      []string           `json:"dialects"`
	SupportsPitch bool               `json:"supportsPitch"`
	PreviewURL    string             `json:"previewURL,omitempty"`
	ResourceID    string             `json:"resourceID"`
	BillingClass  string             `json:"billingClass"`
	Enabled       bool               `json:"enabled"`
	Rank          int                `json:"rank"`
	ProbeText     string             `json:"probeText"`
	Verification  *VoiceVerification `json:"verification,omitempty"`
}

// Verification is written by the synthesis probe, never accepted as an admin
// mutation input. Changing an advertised capability invalidates its evidence.
type VoiceVerification struct {
	PolicySHA256 string    `json:"policySHA256"`
	AudioSHA256  string    `json:"audioSHA256"`
	VerifiedAt   time.Time `json:"verifiedAt"`
	Cases        int       `json:"cases"`
}

type Catalog struct {
	Version        string  `json:"version"`
	DefaultVoiceID string  `json:"defaultVoiceID"`
	Voices         []Voice `json:"voices"`
}

type PublicVoice struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Languages     []string `json:"languages"`
	Styles        []string `json:"styles"`
	Dialects      []string `json:"dialects"`
	SupportsPitch bool     `json:"supportsPitch"`
	PreviewURL    string   `json:"previewURL,omitempty"`
	Revision      string   `json:"revision"`
}

type PublicCatalog struct {
	Version        string        `json:"version"`
	DefaultVoiceID string        `json:"defaultVoiceID"`
	MaxAgeSeconds  int           `json:"maxAgeSeconds"`
	Voices         []PublicVoice `json:"voices"`
}

func validSet(values []string, limit int) bool {
	if len(values) > limit {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func (voice Voice) Validate() error {
	switch voice.BillingClass {
	case "unreviewed", "standard", "premium", "voice-slot", "unavailable":
	default:
		return ErrCatalog
	}
	if !voiceIDPattern.MatchString(voice.ID) || strings.TrimSpace(voice.Name) == "" || utf8.RuneCountInString(voice.Name) > 80 ||
		utf8.RuneCountInString(voice.Description) > 500 || voice.ResourceID != ResourceID ||
		len(voice.Languages) == 0 || !validSet(voice.Languages, 40) || !validSet(voice.Styles, 3) ||
		!slices.Contains(voice.Styles, "natural") || !validSet(voice.Dialects, 8) || voice.Rank < 0 || voice.Rank > 10000 ||
		!utf8.ValidString(voice.ProbeText) || len(voice.ProbeText) > 180 || strings.TrimSpace(voice.ProbeText) == "" {
		return ErrCatalog
	}
	if voice.PreviewURL != "" {
		u, err := url.Parse(voice.PreviewURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(voice.PreviewURL) > 2048 {
			return ErrCatalog
		}
	}
	base := Synthesis{RequestID: "ec298581-b2b5-41a3-8a15-107faeeae531", Text: voice.ProbeText, VoiceID: voice.ID}
	for _, language := range voice.Languages {
		probe := base
		probe.Language = language
		if !probe.Valid() || languageProbeSamples[language] == "" {
			return ErrCatalog
		}
	}
	for _, style := range voice.Styles {
		probe := base
		probe.Style = style
		if !probe.Valid() {
			return ErrCatalog
		}
	}
	for _, dialect := range voice.Dialects {
		probe := base
		probe.Dialect = dialect
		if !probe.Valid() {
			return ErrCatalog
		}
	}
	return nil
}

func (voice Voice) policyHash() string {
	// Presentation-only edits do not invalidate audio or require paid synthesis.
	texts := []string{voice.ProbeText}
	for i, language := range voice.Languages {
		if i > 0 {
			texts = append(texts, languageProbeSamples[language])
		}
	}
	policy := struct {
		ID, Resource, Billing       string
		Texts                       []string
		Languages, Styles, Dialects []string
		Pitch                       bool
	}{
		voice.ID, voice.ResourceID, voice.BillingClass, texts, voice.Languages, voice.Styles, voice.Dialects, voice.SupportsPitch}
	raw, _ := json.Marshal(policy)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (voice Voice) IsVerified() bool {
	v := voice.Verification
	if v == nil {
		return false
	}
	audioHash, err := hex.DecodeString(v.AudioSHA256)
	return voice.Validate() == nil && voice.BillingClass == "standard" && v != nil && !v.VerifiedAt.IsZero() &&
		v.Cases == len(voice.probes()) && v.PolicySHA256 == voice.policyHash() && err == nil && len(audioHash) == sha256.Size
}

func (voice Voice) Allows(input Synthesis) bool {
	return voice.Enabled && voice.IsVerified() && voice.ID == input.VoiceID &&
		(input.Pitch == 0 || voice.SupportsPitch) && slices.Contains(voice.Styles, input.Style) &&
		(input.Dialect == "" || slices.Contains(voice.Dialects, input.Dialect)) &&
		(input.Language == "" || slices.Contains(voice.Languages, input.Language))
}

func (catalog Catalog) ValidatePublication() error {
	if _, err := uuid.Parse(catalog.Version); err != nil {
		return ErrCatalog
	}
	if len(catalog.Voices) == 0 || len(catalog.Voices) > 5000 {
		return ErrCatalog
	}
	seen := map[string]bool{}
	hasDefault := false
	for _, voice := range catalog.Voices {
		if seen[voice.ID] || voice.Validate() != nil {
			return ErrCatalog
		}
		seen[voice.ID] = true
		if voice.Enabled {
			if !voice.IsVerified() {
				return ErrCatalog
			}
			if voice.ID == catalog.DefaultVoiceID {
				hasDefault = true
			}
		}
	}
	if !hasDefault {
		return ErrCatalog
	}
	return nil
}

func (catalog Catalog) Public() (PublicCatalog, error) {
	if err := catalog.ValidatePublication(); err != nil {
		return PublicCatalog{}, err
	}
	voices := slices.Clone(catalog.Voices)
	slices.SortStableFunc(voices, func(a, b Voice) int {
		if a.Rank != b.Rank {
			return a.Rank - b.Rank
		}
		return strings.Compare(a.Name, b.Name)
	})
	result := PublicCatalog{Version: catalog.Version, DefaultVoiceID: catalog.DefaultVoiceID, MaxAgeSeconds: 4 * 60 * 60, Voices: []PublicVoice{}}
	for _, voice := range voices {
		if !voice.Enabled {
			continue
		}
		result.Voices = append(result.Voices, PublicVoice{voice.ID, voice.Name, voice.Description,
			voice.Languages, voice.Styles, append([]string{}, voice.Dialects...), voice.SupportsPitch, voice.PreviewURL, voice.policyHash()})
	}
	return result, nil
}

func (voice Voice) probes() []Synthesis {
	base := Synthesis{Text: voice.ProbeText, VoiceID: voice.ID, Style: "natural", Language: voice.Languages[0]}
	result := []Synthesis{base}
	for _, language := range voice.Languages[1:] {
		probe := base
		probe.Language = language
		probe.Text = languageProbeSamples[language]
		result = append(result, probe)
	}
	for _, style := range voice.Styles {
		if style != "natural" {
			probe := base
			probe.Style = style
			result = append(result, probe)
		}
	}
	for _, dialect := range voice.Dialects {
		probe := base
		probe.Dialect = dialect
		result = append(result, probe)
	}
	if voice.SupportsPitch {
		for _, pitch := range []int{-12, 12} {
			probe := base
			probe.Pitch = pitch
			result = append(result, probe)
		}
	}
	return result
}

// VerifyVoice exercises every advertised control. Callers pass the budgeted
// synthesizer; discovery itself never turns on voices or purchases resources.
func VerifyVoice(ctx context.Context, voice Voice, provider Synthesizer, now time.Time) (Voice, []byte, error) {
	voice.Verification = nil
	if provider == nil || voice.Validate() != nil || voice.BillingClass != "standard" || now.IsZero() {
		return voice, nil, ErrCatalog
	}
	hash := sha256.New()
	var preview []byte
	probes := voice.probes()
	for i, input := range probes {
		input.RequestID = uuid.NewString()
		written := 0
		usage, err := provider.Synthesize(ctx, input, func(pcm []byte) error {
			if len(pcm) > 1<<20-written {
				return io.ErrShortBuffer
			}
			written += len(pcm)
			_, _ = hash.Write(pcm)
			if i == 0 {
				preview = append(preview, pcm...)
			}
			return nil
		})
		if err != nil {
			return voice, nil, err
		}
		if written == 0 || written%2 != 0 || usage.AudioBytes != written {
			return voice, nil, ErrIncomplete
		}
	}
	voice.Verification = &VoiceVerification{PolicySHA256: voice.policyHash(), AudioSHA256: hex.EncodeToString(hash.Sum(nil)), VerifiedAt: now.UTC(), Cases: len(probes)}
	return voice, preview, nil
}
