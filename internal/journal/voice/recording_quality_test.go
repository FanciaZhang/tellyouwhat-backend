package voice

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type recordingQualityCase struct {
	Name              string             `json:"name"`
	Mode              string             `json:"mode"`
	NarratorSpeakerID string             `json:"narratorSpeakerID"`
	Speakers          []RecordingSpeaker `json:"speakers"`
	Utterances        []struct {
		ID      string `json:"id"`
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	} `json:"utterances"`
	Draft              string   `json:"draft"`
	ExpectedIssueCodes []string `json:"expectedIssueCodes"`
	RequiredPhrases    []string `json:"requiredPhrases"`
	ForbiddenPhrases   []string `json:"forbiddenPhrases"`
}

func recordingQualityCases(t *testing.T) []recordingQualityCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/recording_quality_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []recordingQualityCase
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatal("invalid recording quality cases", err)
	}
	return cases
}

func qualityAnalysis(tc recordingQualityCase) RecordingAnalysis {
	analysis := RecordingAnalysis{
		Version:      RecordingAnalysisVersion,
		TaskID:       uuid.NewString(),
		Milliseconds: len(tc.Utterances) * 1_000,
	}
	for index, source := range tc.Utterances {
		analysis.Text += source.Text
		analysis.Utterances = append(analysis.Utterances, RecordingUtterance{
			ID:                source.ID,
			Speaker:           source.Speaker,
			StartMilliseconds: index * 1_000,
			EndMilliseconds:   (index + 1) * 1_000,
			Text:              source.Text,
		})
	}
	return analysis
}

func TestRecordingQualityGoldenCasesProduceExpectedReviewSignals(t *testing.T) {
	for _, tc := range recordingQualityCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			issues := ReviewRecordingDraft(qualityAnalysis(tc), tc.Draft)
			actual := map[string]bool{}
			for _, issue := range issues {
				actual[issue.Code] = true
			}
			expected := map[string]bool{}
			for _, code := range tc.ExpectedIssueCodes {
				expected[code] = true
			}
			if len(actual) != len(expected) {
				t.Fatalf("review signal count=%d want=%d", len(actual), len(expected))
			}
			for code := range expected {
				if !actual[code] {
					t.Fatalf("missing review signal %s", code)
				}
			}
		})
	}
}

// This suite is opt-in because it calls the configured paid provider. The
// synthetic fixtures contain no user data and exercise the same strict schema,
// passage provenance, emotion contract and fidelity review used in production.
func TestLiveRecordingQualityGoldenCases(t *testing.T) {
	if os.Getenv("JOURNAL_VOICE_QUALITY_LIVE_CHECK") != "1" {
		t.Skip("explicit live quality opt-in required")
	}
	baseURL := os.Getenv("JOURNAL_ARK_BASE_URL")
	apiKey := os.Getenv("JOURNAL_ARK_API_KEY")
	model := os.Getenv("JOURNAL_VOICE_MODEL")
	if baseURL == "" || apiKey == "" || model == "" {
		t.Fatal("live quality provider configuration is incomplete")
	}

	for _, tc := range recordingQualityCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			analysis := qualityAnalysis(tc)
			blockID := uuid.NewString()
			mode := tc.Mode
			if mode == "" {
				mode = "stream"
			}
			snapshot := Snapshot{
				Revision:     0,
				WritingStyle: StyleNatural,
				Blocks:       []Block{{ID: blockID, Text: ""}},
				Transcript:   analysis.Text,
				RecordingContext: &RecordingContext{
					Mode:              mode,
					NarratorSpeakerID: tc.NarratorSpeakerID,
					Speakers:          tc.Speakers,
					Analysis:          analysis,
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			result, err := (ArkRewriter{
				BaseURL: baseURL,
				APIKey:  apiKey,
				Model:   model,
			}).Rewrite(ctx, snapshot, 1)
			cancel()
			if err != nil {
				t.Fatal("live quality rewrite failed", err)
			}
			blocks, err := ApplyRevision(snapshot, result.Revision)
			if err != nil {
				t.Fatal("live quality revision was invalid", err)
			}
			draft := ""
			for _, block := range blocks {
				draft += block.Text + "\n"
			}
			if issues := ReviewRecordingDraft(analysis, draft); len(issues) != 0 {
				t.Fatalf("live rewrite triggered %d fidelity review signals", len(issues))
			}
			for _, phrase := range tc.RequiredPhrases {
				if !strings.Contains(draft, phrase) {
					t.Fatalf("live rewrite lost required phrase %q", phrase)
				}
			}
			for _, phrase := range tc.ForbiddenPhrases {
				if strings.Contains(draft, phrase) {
					t.Fatalf("live rewrite introduced forbidden phrase %q", phrase)
				}
			}
		})
	}
}
