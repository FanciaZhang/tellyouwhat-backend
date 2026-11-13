package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

// Explicit real-provider checks use synthetic sources and the configured policy.
func TestLivePolishFidelity(t *testing.T) {
	if os.Getenv("JOURNAL_POLISH_FIDELITY_LIVE") != "1" {
		t.Skip("explicit synthetic real-provider acceptance only")
	}
	root := os.Getenv("JOURNAL_POLISH_FIDELITY_EVIDENCE")
	if !filepath.IsAbs(root) || os.Getenv("JOURNAL_ARK_API_KEY") == "" {
		t.Fatal("private evidence directory and configured provider required")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("existing private evidence directory required")
	}
	var revision promptconfig.Revision
	raw, err := os.ReadFile(os.Getenv("JOURNAL_POLISH_FIDELITY_POLICY"))
	if err != nil || json.Unmarshal(raw, &revision) != nil {
		t.Fatal("configured policy unavailable")
	}
	var cases []struct {
		Name                string
		Turns               []string
		Required, Forbidden []string
		QuestionsRequired   bool
	}
	raw, err = os.ReadFile("testdata/polish_fidelity.json")
	if err != nil || json.Unmarshal(raw, &cases) != nil || len(cases) != 9 {
		t.Fatal("bounded synthetic cases unavailable")
	}
	for _, sample := range cases {
		for _, pattern := range append(append([]string{}, sample.Required...), sample.Forbidden...) {
			if _, err := regexp.Compile(pattern); err != nil {
				t.Fatal("invalid synthetic assertion", sample.Name)
			}
		}
	}
	price := costcontrol.TokenPrice{InputNanosPerMillionTokens: 10_000_000_000, OutputNanosPerMillionTokens: 100_000_000_000}
	budget, err := costcontrol.New(costcontrol.NewMemoryStore(), costcontrol.Limits{MonthlyBudgetNanos: 2 * costcontrol.NanosPerCNY, MaxConcurrent: 1, LeaseDuration: 2 * time.Minute}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	revision.Policy.Journal.Voice.Parameters.Price = &price
	model := NewBudgetedRewriter(ArkRewriter{BaseURL: os.Getenv("JOURNAL_ARK_BASE_URL"), APIKey: os.Getenv("JOURNAL_ARK_API_KEY")}, budget, "polish-fidelity-acceptance", price)
	for _, sample := range cases {
		t.Run(sample.Name, func(t *testing.T) {
			snapshot := comparisonSnapshot(sample.Turns, false)
			author := uuid.NewSHA1(uuid.NameSpaceOID, []byte(sample.Name+"/author")).String()
			snapshot.Narrator.PersonID = author
			for i := range snapshot.Polish.Targets {
				target := &snapshot.Polish.Targets[i]
				target.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(sample.Name+"/target/"+string(rune('a'+i)))).String()
				source := uuid.NewSHA1(uuid.NameSpaceOID, []byte(target.ID+"/source")).String()
				target.SourceIDs, target.Turns[0].ID, target.Turns[0].PersonID = []string{source}, source, author
				snapshot.Blocks[i].ID = target.ID
			}
			ctx, cancel := context.WithTimeout(promptconfig.WithRevision(t.Context(), revision), 95*time.Second)
			defer cancel()
			request, err := PrepareRewrite(ctx, snapshot, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			result, callErr := model.Rewrite(ctx, snapshot, 1)
			text, questions := "", []string{}
			if result.Polish != nil {
				for _, p := range result.Polish.Paragraphs {
					text += p.Text + "\n"
				}
				questions = result.Polish.Questions
			}
			checks := map[string]bool{}
			for _, p := range sample.Required {
				checks["requires:"+p] = regexp.MustCompile(p).MatchString(text)
			}
			for _, p := range sample.Forbidden {
				checks["excludes:"+p] = !regexp.MustCompile(p).MatchString(text)
			}
			if sample.QuestionsRequired {
				checks["asks_unknown_name"] = len(questions) > 0
			}
			passed := callErr == nil && result.Polish != nil
			for name, ok := range checks {
				if !ok {
					passed = false
					t.Errorf("%s", name)
				}
			}
			record := map[string]any{"synthetic": true, "appAcceptance": false, "source": sample.Turns, "request": json.RawMessage(request.Body), "text": strings.TrimSpace(text), "questions": questions, "result": result, "diagnostics": result.Diagnostics, "checks": checks, "passed": passed, "milliseconds": time.Since(started).Milliseconds()}
			raw, err := json.MarshalIndent(record, "", "  ")
			if err != nil || os.WriteFile(filepath.Join(root, sample.Name+".json"), raw, 0600) != nil {
				t.Fatal("evidence write failed")
			}
			if callErr != nil || result.Polish == nil {
				t.Error("configured provider or production decoding failed")
			}
			t.Logf("passed=%t model=%s", passed, result.Model)
		})
	}
}
