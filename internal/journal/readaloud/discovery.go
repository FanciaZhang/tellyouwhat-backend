package readaloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/volcengine/volcengine-go-sdk/service/speechsaasprod"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
)

var (
	ErrDiscovery            = errors.New("voice inventory unavailable")
	ErrDiscoveryCredentials = errors.New("voice inventory credentials unavailable")
)

type speakerInventoryAPI interface {
	ListSpeakersWithContext(volcengine.Context, *speechsaasprod.ListSpeakersInput, ...request.Option) (*speechsaasprod.ListSpeakersOutput, error)
}
type Discovery struct{ api speakerInventoryAPI }
type DiscoveryIssue struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type DiscoveredVoices struct {
	Voices        []VoiceConfiguration `json:"voices"`
	Issues        []DiscoveryIssue     `json:"issues"`
	ProviderCount int                  `json:"providerCount"`
}
type boundedInventoryTransport struct{ base http.RoundTripper }
type inventoryBody struct {
	io.Reader
	io.Closer
}

func (t boundedInventoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err == nil {
		response.Body = inventoryBody{io.LimitReader(response.Body, 4<<20), response.Body}
	}
	return response, err
}

// Inventory credentials are independent of the paid synthesis key. No default
// profile, global environment credential, activation or purchase is used.
func NewDiscoveryFromFile(path string) (*Discovery, error) {
	raw, err := privateFile(path)
	if err != nil {
		return nil, ErrDiscoveryCredentials
	}
	var secret struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&secret) != nil || secret.AccessKey == "" || secret.SecretKey == "" || d.Decode(new(any)) != io.EOF {
		return nil, ErrDiscoveryCredentials
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: boundedInventoryTransport{http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cfg := volcengine.NewConfig().WithRegion("cn-beijing").WithEndpoint("https://open.volcengineapi.com").WithCredentials(credentials.NewStaticCredentials(secret.AccessKey, secret.SecretKey, "")).WithHTTPClient(client).WithMaxRetries(0)
	s, err := session.NewSession(cfg)
	if err != nil {
		return nil, ErrDiscovery
	}
	return &Discovery{api: speechsaasprod.New(s)}, nil
}

func (d *Discovery) Candidates(ctx context.Context) (DiscoveredVoices, error) {
	result := DiscoveredVoices{Voices: []VoiceConfiguration{}, Issues: []DiscoveryIssue{}}
	if d == nil || d.api == nil {
		return result, ErrDiscovery
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	seen := map[string]bool{}
	expected := -1
	for page := int32(1); page <= 50; page++ {
		response, err := d.api.ListSpeakersWithContext(ctx, &speechsaasprod.ListSpeakersInput{ResourceIDs: []*string{volcengine.String(ResourceID)}, Page: volcengine.Int32(page), Limit: volcengine.String("100")})
		if err != nil || response == nil || response.Total == nil || *response.Total < 0 || *response.Total > 5000 || len(response.Speakers) > 100 {
			return DiscoveredVoices{}, ErrDiscovery
		}
		if expected < 0 {
			expected = int(*response.Total)
		} else if expected != int(*response.Total) {
			return DiscoveredVoices{}, ErrDiscovery
		}
		for _, speaker := range response.Speakers {
			if speaker == nil {
				return DiscoveredVoices{}, ErrDiscovery
			}
			id := volcengine.StringValue(speaker.VoiceType)
			if !voiceIDPattern.MatchString(id) || seen[id] {
				return DiscoveredVoices{}, ErrDiscovery
			}
			seen[id] = true
			voice, issue := discoveredVoice(speaker)
			if issue != "" {
				result.Issues = append(result.Issues, DiscoveryIssue{id, issue})
			} else {
				result.Voices = append(result.Voices, Configuration(voice))
			}
		}
		result.ProviderCount = len(seen)
		if len(seen) == expected {
			return result, nil
		}
		if len(seen) > expected || len(response.Speakers) == 0 {
			return DiscoveredVoices{}, ErrDiscovery
		}
	}
	return DiscoveredVoices{}, ErrDiscovery
}

func inventoryLanguage(value string) string {
	code := strings.ToLower(strings.ReplaceAll(value, "_", "-"))
	switch code {
	case "zh", "zh-cn", "cmn":
		return "zh-cn"
	case "en-us", "en-gb":
		return "en"
	case "es", "es-es":
		return "es-es"
	case "es-419":
		return "es-mx"
	case "nb", "nb-no":
		return "no"
	case "tl", "fil-ph":
		return "fil"
	}
	for _, language := range []string{"ja", "ko", "fr", "de", "id", "it", "th", "vi", "ru", "ms", "ar", "pl", "tr", "sv", "nl", "uk", "fi", "da", "cs", "hu", "el", "ro", "hi"} {
		if code == language || strings.HasPrefix(code, language+"-") {
			return language
		}
	}
	return code
}
func boundedProbeText(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		piece := string(r)
		if b.Len()+len(piece) > 180 {
			break
		}
		b.WriteString(piece)
	}
	return b.String()
}
func discoveredVoice(s *speechsaasprod.SpeakerForListSpeakersOutput) (Voice, string) {
	v := Voice{ID: volcengine.StringValue(s.VoiceType), Name: strings.TrimSpace(volcengine.StringValue(s.Name)), Description: volcengine.StringValue(s.Description),
		ResourceID: volcengine.StringValue(s.ResourceID), BillingClass: "unreviewed", Styles: []string{"natural"}, Dialects: []string{}, Rank: 1000, PreviewURL: volcengine.StringValue(s.TrialURL)}
	if v.ResourceID != ResourceID {
		return v, "unsupported_resource"
	}
	for _, language := range s.Languages {
		if language == nil {
			continue
		}
		code := inventoryLanguage(volcengine.StringValue(language.Language))
		probe := Synthesis{RequestID: "ec298581-b2b5-41a3-8a15-107faeeae531", VoiceID: v.ID, Text: "test", Language: code}
		if !probe.Valid() || code == "" {
			continue
		}
		if !slices.Contains(v.Languages, code) {
			v.Languages = append(v.Languages, code)
		}
		if v.ProbeText == "" {
			v.ProbeText = boundedProbeText(volcengine.StringValue(language.Text))
		}
	}
	if len(v.Languages) == 0 {
		return v, "unsupported_language"
	}
	if v.ProbeText == "" {
		v.ProbeText = languageProbeSamples[v.Languages[0]]
	}
	if v.Validate() != nil {
		return v, "invalid_metadata"
	}
	return v, ""
}
