// Package readaloud provides bounded, verbatim text-to-speech for Journal.
package readaloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	Endpoint          = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
	ResourceID        = "seed-tts-2.0"
	SampleRate        = 24_000
	MaximumTextBytes  = 900
	MaximumAudioBytes = SampleRate * 2 * 240
)

var (
	ErrInput       = errors.New("invalid read-aloud input")
	ErrProvider    = errors.New("read-aloud provider unavailable")
	ErrIncomplete  = errors.New("incomplete read-aloud audio")
	voiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// ProviderError never includes the upstream response body, which may echo text.
type ProviderError struct {
	Status, Code int
	LogID        string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("read-aloud provider failed (HTTP %d, code %d)", e.Status, e.Code)
}
func (e *ProviderError) Unwrap() error { return ErrProvider }

type Synthesis struct {
	RequestID string
	Text      string
	VoiceID   string
	Pitch     int
	Style     string
	Dialect   string
	Language  string
}

type SynthesisUsage struct {
	Characters int
	UsageKnown bool
	AudioBytes int
	LogID      string
}

type Synthesizer interface {
	Synthesize(context.Context, Synthesis, func([]byte) error) (SynthesisUsage, error)
}

type Provider struct {
	endpoint, apiKey string
	client           *http.Client
}

func NewProvider(endpoint, apiKey string, client *http.Client) (*Provider, error) {
	if endpoint == "" {
		endpoint = Endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(apiKey) == "" {
		return nil, ErrInput
	}
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	// A provider redirect must not forward the key to another origin.
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{endpoint: endpoint, apiKey: apiKey, client: &copy}, nil
}

func (input Synthesis) Valid() bool {
	if id, err := uuid.Parse(input.RequestID); err != nil || id == uuid.Nil {
		return false
	}
	if !utf8.ValidString(input.Text) || len(input.Text) > MaximumTextBytes || strings.TrimSpace(input.Text) == "" ||
		!voiceIDPattern.MatchString(input.VoiceID) || input.Pitch < -12 || input.Pitch > 12 {
		return false
	}
	switch input.Style {
	case "", "natural", "calm", "storytelling":
	default:
		return false
	}
	switch input.Dialect {
	case "", "beijing", "dongbei", "henan", "shaanxi", "shanghai", "sichuan", "tianjin", "yue":
	default:
		return false
	}
	switch input.Language {
	case "", "zh-cn", "en", "ja", "ko", "fr", "de", "es-mx", "es-es", "pt-br", "pt", "id", "it", "th", "vi", "ru", "fil", "ms", "ar", "pl", "tr", "sv", "nl", "no", "uk", "fi", "da", "cs", "hu", "el", "ro", "hi":
	default:
		return false
	}
	return true
}

func (provider *Provider) Synthesize(ctx context.Context, input Synthesis, emit func([]byte) error) (result SynthesisUsage, err error) {
	if provider == nil || !input.Valid() || emit == nil {
		return result, ErrInput
	}
	additions := map[string]any{"post_process": map[string]int{"pitch": input.Pitch}}
	if input.Dialect != "" {
		additions["explicit_dialect"] = input.Dialect
	}
	if input.Language != "" {
		additions["explicit_language"] = input.Language
	}
	switch input.Style {
	case "calm":
		additions["context_texts"] = []string{"请平静、舒缓地忠实朗读，不增删文字。"}
	case "storytelling":
		additions["context_texts"] = []string{"请用自然讲述的语气忠实朗读，不增删文字。"}
	}
	extra, _ := json.Marshal(additions)
	body, _ := json.Marshal(map[string]any{"req_params": map[string]any{
		"text": input.Text, "speaker": input.VoiceID, "additions": string(extra),
		"audio_params": map[string]any{"format": "pcm", "sample_rate": SampleRate, "speech_rate": 0},
	}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, ErrInput
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", provider.apiKey)
	request.Header.Set("X-Api-Resource-Id", ResourceID)
	request.Header.Set("X-Api-Request-Id", input.RequestID)
	request.Header.Set("X-Control-Require-Usage-Tokens-Return", "*")
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrProvider
	}
	defer response.Body.Close()
	logID := response.Header.Get("X-Tt-Logid")
	if len(logID) <= 128 && !strings.ContainsAny(logID, "\r\n") {
		result.LogID = logID
	}
	if response.StatusCode != http.StatusOK {
		return result, &ProviderError{Status: response.StatusCode, LogID: result.LogID}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, int64(MaximumAudioBytes*2)))
	for {
		var frame struct {
			Code  *int   `json:"code"`
			Data  []byte `json:"data"`
			Usage *struct {
				TextWords *int `json:"text_words"`
			} `json:"usage"`
		}
		if err := decoder.Decode(&frame); err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrIncomplete
		}
		if frame.Code == nil {
			return result, ErrIncomplete
		}
		switch *frame.Code {
		case 0, 20_000_000:
			if len(frame.Data) > 0 {
				if len(frame.Data) > MaximumAudioBytes-result.AudioBytes {
					return result, ErrIncomplete
				}
				result.AudioBytes += len(frame.Data)
				if err := emit(frame.Data); err != nil {
					return result, err
				}
			}
			if *frame.Code == 20_000_000 {
				if result.AudioBytes == 0 || result.AudioBytes%2 != 0 {
					return result, ErrIncomplete
				}
				if frame.Usage != nil && frame.Usage.TextWords != nil {
					count := *frame.Usage.TextWords
					if count <= 0 || count > MaximumTextBytes*2 {
						return result, ErrIncomplete
					}
					result.Characters = count
					result.UsageKnown = true
				}
				return result, nil
			}
		default:
			return result, &ProviderError{Status: response.StatusCode, Code: *frame.Code, LogID: result.LogID}
		}
	}
}
