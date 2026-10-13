package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

type liveImageTransport func(*http.Request) (*http.Response, error)

func (f liveImageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Explicit synthetic integration. The relay retains provider credentials on
// the server and accepts only the configured text and image models.
func TestLiveServerOwnedIllustrationPrompts(t *testing.T) {
	if os.Getenv("JOURNAL_SERVER_PROMPTS_LIVE") != "1" {
		t.Skip("explicit real-provider acceptance only")
	}
	base := os.Getenv("JOURNAL_ARK_BASE_URL")
	client := New(Config{BaseURL: base, LiteModel: os.Getenv("JOURNAL_ARK_LITE_MODEL_ID"), ProModel: os.Getenv("JOURNAL_ARK_PRO_MODEL_ID")}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	data, err := os.ReadFile(os.Getenv("JOURNAL_ORGANIZE_POLICY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	var runtime promptconfig.Revision
	if err = json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	ctx = promptconfig.WithRevision(ctx, runtime)
	input := contracts.OrganizeRequest{Title: "河边散步", Body: "傍晚我带小狗去河边散步。小狗停在一棵柳树旁，看着一只白色蝴蝶从草丛里飞起。夕阳把水面照成了金色。", IllustrationSuggestionsEnabled: true}
	result, err := client.Organize(ctx, input, false)
	if err != nil || result.Value.IllustrationSuggestion == nil {
		t.Fatalf("scene request failed: %v %+v", err, result)
	}
	scene := result.Value.IllustrationSuggestion
	if !scene.Grounded([]string{input.Body}) || len(result.Value.Tags) != 0 {
		t.Fatal("scene crossed its feature boundary")
	}
	brief := illustration.Brief{Title: scene.Summary, Details: []illustration.Detail{{Kind: "subject", Text: scene.Subject}, {Kind: "setting", Text: scene.Setting}, {Kind: "style", Text: "柔和水彩"}}}
	if err := brief.Validate(); err != nil {
		t.Fatal(err)
	}
	relay, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	transport := liveImageTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = relay.Scheme
		u.Host = relay.Host
		copy.URL = &u
		return http.DefaultTransport.RoundTrip(copy)
	})
	generator, err := illustration.NewProvider("https://synthetic-provider.invalid", "server-held-key", os.Getenv("JOURNAL_IMAGE_MODEL_ID"), transport)
	if err != nil {
		t.Fatal(err)
	}
	image, err := generator.Generate(ctx, illustration.Input{Prompt: brief.Compile(ctx)})
	if err != nil {
		t.Fatal(err)
	}
	if image.Width <= image.Height {
		t.Fatalf("expected landscape image: %dx%d", image.Width, image.Height)
	}
	evidence := os.Getenv("JOURNAL_ORGANIZE_EVIDENCE_DIR")
	if evidence == "" {
		t.Fatal("evidence directory required")
	}
	raw, _ := json.MarshalIndent(map[string]any{"request": input, "scene": scene, "brief": brief, "model": result.Model, "inputTokens": result.InputTokens, "outputTokens": result.OutputTokens, "width": image.Width, "height": image.Height, "mime": image.MIME}, "", "  ")
	if err := os.WriteFile(filepath.Join(evidence, "server-prompts-scene.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "server-prompts-image.jpg"), image.Image, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("real scene and image completed: %dx%d (%d bytes)", image.Width, image.Height, len(image.Image))
}
