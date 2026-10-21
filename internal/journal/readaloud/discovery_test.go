package readaloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/volcengine/volcengine-go-sdk/service/speechsaasprod"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/request"
)

type inventoryFixture struct {
	pages []*speechsaasprod.ListSpeakersOutput
	calls int
	err   error
}

func (f *inventoryFixture) ListSpeakersWithContext(_ volcengine.Context, input *speechsaasprod.ListSpeakersInput, _ ...request.Option) (*speechsaasprod.ListSpeakersOutput, error) {
	if volcengine.StringValue(input.Limit) != "100" || len(input.ResourceIDs) != 1 || *input.ResourceIDs[0] != ResourceID || int(*input.Page) != f.calls+1 {
		return nil, ErrDiscovery
	}
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.pages[f.calls-1], nil
}
func inventoryVoice(id string) *speechsaasprod.SpeakerForListSpeakersOutput {
	return &speechsaasprod.SpeakerForListSpeakersOutput{VoiceType: volcengine.String(id), Name: volcengine.String("候选声音"), ResourceID: volcengine.String(ResourceID), TrialURL: volcengine.String("https://example.com/public-voice.mp3"), Languages: []*speechsaasprod.LanguageForListSpeakersOutput{{Language: volcengine.String("zh_CN"), Text: volcengine.String("今天沿着河岸散步。")}, {Language: volcengine.String("en-US")}}}
}
func TestVoiceDiscoveryPaginatesWithoutEnablingOrClaimingPriceVerification(t *testing.T) {
	first, second := inventoryVoice("tl_male_jonathan-palad_uranus_bigtts"), inventoryVoice("voice_second")
	f := &inventoryFixture{pages: []*speechsaasprod.ListSpeakersOutput{{Total: volcengine.Int32(2), Speakers: []*speechsaasprod.SpeakerForListSpeakersOutput{first}}, {Total: volcengine.Int32(2), Speakers: []*speechsaasprod.SpeakerForListSpeakersOutput{second}}}}
	r, err := (&Discovery{api: f}).Candidates(context.Background())
	if err != nil || f.calls != 2 || len(r.Voices) != 2 || r.ProviderCount != 2 {
		t.Fatal("incomplete inventory", r, err)
	}
	for _, v := range r.Voices {
		if v.Enabled || v.BillingClass != "unreviewed" || len(v.Languages) != 2 || v.SupportsPitch || len(v.Styles) != 1 {
			t.Fatal("discovery granted unverified capabilities", v)
		}
	}
}
func TestVoiceDiscoveryReportsUnsupportedCandidatesAndDiscardsPartialFailure(t *testing.T) {
	unsupported := inventoryVoice("voice_unsupported")
	unsupported.Languages[0].Language = volcengine.String("unknown-language")
	unsupported.Languages = unsupported.Languages[:1]
	f := &inventoryFixture{pages: []*speechsaasprod.ListSpeakersOutput{{Total: volcengine.Int32(1), Speakers: []*speechsaasprod.SpeakerForListSpeakersOutput{unsupported}}}}
	r, err := (&Discovery{api: f}).Candidates(context.Background())
	if err != nil || len(r.Voices) != 0 || len(r.Issues) != 1 || r.Issues[0].Reason != "unsupported_language" {
		t.Fatal("unsupported metadata silently misrepresented", r, err)
	}
	f = &inventoryFixture{err: errors.New("upstream private account details")}
	if _, err := (&Discovery{api: f}).Candidates(context.Background()); err != ErrDiscovery {
		t.Fatal("provider error leaked", err)
	}
	voice := inventoryVoice("voice_duplicate")
	f = &inventoryFixture{pages: []*speechsaasprod.ListSpeakersOutput{{Total: volcengine.Int32(2), Speakers: []*speechsaasprod.SpeakerForListSpeakersOutput{voice}}, {Total: volcengine.Int32(2), Speakers: []*speechsaasprod.SpeakerForListSpeakersOutput{voice}}}}
	if _, err := (&Discovery{api: f}).Candidates(context.Background()); err == nil {
		t.Fatal("changing/duplicate pages accepted")
	}
}
func TestVoiceDiscoveryRejectsUnsafeCredentialFilesAndBoundsProbeText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(path, []byte(`{"accessKey":"fixture","secretKey":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiscoveryFromFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiscoveryFromFile(path); err != ErrDiscoveryCredentials {
		t.Fatal("publicly readable credential admitted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDiscoveryFromFile(link); err != ErrDiscoveryCredentials {
		t.Fatal("credential symlink admitted")
	}
	text := boundedProbeText(strings.Repeat("测试🙂", 100))
	if len(text) > 180 || strings.ContainsRune(text, '\uFFFD') {
		t.Fatal("invalid UTF-8 probe boundary")
	}
}

func TestDiscoveryFallbackUsesNativeLanguagePublicSample(t *testing.T) {
	speaker := inventoryVoice("fr_female_fixture_uranus_bigtts")
	speaker.Languages = speaker.Languages[:1]
	speaker.Languages[0].Language = volcengine.String("fr")
	speaker.Languages[0].Text = nil
	voice, issue := discoveredVoice(speaker)
	if issue != "" || voice.ProbeText == "" || voice.ProbeText == "A quiet day can hold a moment worth remembering." {
		t.Fatal("native language fallback is unavailable", voice, issue)
	}
}
