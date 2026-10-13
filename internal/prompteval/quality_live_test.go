package prompteval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/journal/illustration"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

type qualityLiveTransport func(*http.Request) (*http.Response, error)

func (f qualityLiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Synthetic provider acceptance uses exactly the production evaluation execution path.
func TestLiveJournalRequiredQualitySamples(t *testing.T) {
	if os.Getenv("JOURNAL_PUBLICATION_LIVE") != "1" {
		t.Skip("explicit real-provider acceptance only")
	}
	raw, err := os.ReadFile(os.Getenv("JOURNAL_ORGANIZE_POLICY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	var revision promptconfig.Revision
	if err = json.Unmarshal(raw, &revision); err != nil {
		t.Fatal(err)
	}
	revision.ID = uuid.NewString()
	revision.PublishedAt = nil
	revision.Policy.Journal.Organize.Prompt = promptconfig.OrganizePrompt
	revision.Policy.Journal.IllustrationSuggestionPrompt = promptconfig.IllustrationSuggestionPrompt
	revision.Policy.Journal.IllustrationGenerationPrompt = promptconfig.IllustrationGenerationPrompt
	if err = revision.Policy.Validate("journal"); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("JOURNAL_ORGANIZE_EVIDENCE_DIR")
	if root == "" {
		t.Fatal("evidence directory required")
	}
	raw, _ = json.MarshalIndent(revision, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "candidate-policy.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("JOURNAL_ARK_BASE_URL")
	destination, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	transport := qualityLiveTransport(func(r *http.Request) (*http.Response, error) {
		request := r.Clone(r.Context())
		u := *r.URL
		u.Scheme = destination.Scheme
		u.Host = destination.Host
		request.URL = &u
		request.Host = destination.Host
		return http.DefaultTransport.RoundTrip(request)
	})
	images, err := illustration.NewProvider("https://provider.invalid", "relay", os.Getenv("JOURNAL_IMAGE_MODEL_ID"), transport)
	if err != nil {
		t.Fatal(err)
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 9_600_000_000, OutputNanosPerMillionTokens: 48_000_000_000}
	engine := Engine{Provider: provider.Config{BaseURL: base, LiteModel: revision.Policy.Journal.Organize.Lite.Model, ProModel: revision.Policy.Journal.Organize.Pro.Model}, Price: price, Images: images, ImageCostNanos: ImageReservationNanos}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 30e9, MaxConcurrent: 2, LeaseDuration: 10 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range RequiredQualitySamples() {
		t.Run(sample.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			output := engine.execute(promptconfig.WithRevision(ctx, revision), budget, sample, Candidate{Revision: revision}, 0)
			raw, _ := json.MarshalIndent(map[string]any{"sample": sample, "output": output}, "", "  ")
			if err = os.WriteFile(filepath.Join(root, sample.ID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if len(output.Image) > 0 {
				if err = os.WriteFile(filepath.Join(root, "publication-image.jpg"), output.Image, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if output.Error != "" {
				t.Fatal(output.Error)
			}
			for _, check := range output.Checks {
				if !check.Passed {
					t.Errorf("%s: %s", check.Name, check.Detail)
				}
			}
			t.Logf("model=%s duration=%dms tokens=%d/%d checks=%d", output.Model, output.Milliseconds, output.InputTokens, output.OutputTokens, len(output.Checks))
		})
	}
}
