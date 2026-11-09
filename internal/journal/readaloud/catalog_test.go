package readaloud

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type catalogSynthesizer struct {
	calls   []Synthesis
	failure error
}

func (s *catalogSynthesizer) Synthesize(_ context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	s.calls = append(s.calls, input)
	if s.failure != nil {
		return SynthesisUsage{}, s.failure
	}
	err := emit([]byte{1, 2, 3, 4})
	return SynthesisUsage{AudioBytes: 4}, err
}

func draftVoice() Voice {
	return Voice{ID: "zh_female_xiaohe_uranus_bigtts", Name: "小何", Languages: []string{"zh-cn", "en"},
		Styles: []string{"natural", "calm"}, Dialects: []string{"yue"}, SupportsPitch: true,
		ResourceID: ResourceID, BillingClass: "standard", ProbeText: "今天的风很轻，适合停下来听一听。"}
}

func TestCatalogRequiresRealProbeEvidenceForEnabledCapabilities(t *testing.T) {
	provider := &catalogSynthesizer{}
	voice, audio, err := VerifyVoice(context.Background(), draftVoice(), provider, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if voice.Enabled || !voice.IsVerified() || len(provider.calls) != 6 || provider.calls[1].Language != "en" || provider.calls[1].Text == voice.ProbeText || len(audio) != 4 {
		t.Fatal("probe did not cover advertised controls or enabled the voice implicitly")
	}
	voice.Enabled = true
	input := validSynthesis()
	input.Language = "zh-cn"
	if !voice.Allows(input) {
		t.Fatal("verified normal voice denied")
	}
	input.Style = "unadvertised"
	if voice.Allows(input) {
		t.Fatal("unadvertised option admitted")
	}
	voice.Dialects = append(voice.Dialects, "beijing")
	if voice.IsVerified() {
		t.Fatal("new capability reused old verification")
	}
}

func TestCatalogPublicationKeepsLastUsableDefaultAndRejectsCostSurprises(t *testing.T) {
	voice, _, _ := VerifyVoice(context.Background(), draftVoice(), &catalogSynthesizer{}, time.Now())
	voice.Enabled = true
	base := Catalog{Version: uuid.NewString(), DefaultVoiceID: voice.ID, Voices: []Voice{voice}}
	if err := base.ValidatePublication(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Catalog){
		func(c *Catalog) { c.Voices = nil }, func(c *Catalog) { c.DefaultVoiceID = "missing" },
		func(c *Catalog) { c.Voices[0].Enabled = false }, func(c *Catalog) { c.Voices[0].BillingClass = "voice-slot" },
		func(c *Catalog) { c.Voices[0].Verification = nil }, func(c *Catalog) { c.Voices = append(c.Voices, c.Voices[0]) },
	} {
		raw, _ := json.Marshal(base)
		var changed Catalog
		_ = json.Unmarshal(raw, &changed)
		mutate(&changed)
		if changed.ValidatePublication() == nil {
			t.Fatal("invalid catalog can replace active selection")
		}
	}
}

func TestPublicCatalogExcludesUnpublishedVoicesAndInternalFields(t *testing.T) {
	voice, _, _ := VerifyVoice(context.Background(), draftVoice(), &catalogSynthesizer{}, time.Now())
	voice.Enabled = true
	private := draftVoice()
	private.ID = "zh_male_m191_uranus_bigtts"
	private.Name = "候选声音"
	result, err := (Catalog{Version: uuid.NewString(), DefaultVoiceID: voice.ID, Voices: []Voice{private, voice}}).Public()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if len(result.Voices) != 1 || result.MaxAgeSeconds != 14400 {
		t.Fatal("incorrect public catalog")
	}
	for _, forbidden := range []string{"probeText", "verification", "billingClass", "候选声音"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("internal catalog field leaked: %s", forbidden)
		}
	}
	voice.Name = "小何 2.0"
	if !voice.IsVerified() {
		t.Fatal("display name edit caused unnecessary paid probe")
	}
}

func TestFailedProbeCannotLeavePublishableEvidence(t *testing.T) {
	voice, _, _ := VerifyVoice(context.Background(), draftVoice(), &catalogSynthesizer{}, time.Now())
	voice, audio, err := VerifyVoice(context.Background(), voice, &catalogSynthesizer{failure: ErrProvider}, time.Now())
	if !errors.Is(err, ErrProvider) || voice.IsVerified() || len(audio) != 0 {
		t.Fatal("failed verification retained stale proof or audio")
	}
}

func TestAdditionalLanguageProbesUseMatchingPublicTextAndBindEvidence(t *testing.T) {
	voice := draftVoice()
	voice.Languages = []string{"zh-cn", "en", "fr"}
	for _, probe := range voice.probes()[1:3] {
		if strings.ContainsAny(probe.Text, "今天的风很轻，适合停下来听一听。") || probe.Text == voice.ProbeText {
			t.Fatal("language probe reused Chinese text", probe.Language)
		}
		probe.RequestID = uuid.NewString()
		if !probe.Valid() {
			t.Fatal("invalid language sample", probe.Language)
		}
	}
	verified, _, err := VerifyVoice(context.Background(), voice, &catalogSynthesizer{}, time.Now())
	if err != nil || !verified.IsVerified() {
		t.Fatal("language samples could not be verified", err)
	}
	before := verified.policyHash()
	verified.ProbeText = "换一句公开短句。"
	if before == verified.policyHash() || verified.IsVerified() {
		t.Fatal("changed sample reused earlier evidence")
	}
}

func TestPublicLanguageSamplesAreBoundedAndAcceptedByProvider(t *testing.T) {
	for language, text := range languageProbeSamples {
		input := validSynthesis()
		input.Language = language
		input.Text = text
		voice := draftVoice()
		voice.Languages = []string{language}
		voice.ProbeText = text
		if !input.Valid() || voice.Validate() != nil {
			t.Fatal("invalid public sample", language)
		}
	}
}
