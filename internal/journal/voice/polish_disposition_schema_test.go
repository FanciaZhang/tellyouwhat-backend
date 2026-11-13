package voice

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestPolishSchemaCannotDiscardConfirmedAuthorAsNoise(t *testing.T) {
	p := *comparisonSnapshot([]string{"今天见到小林。回。"}, false).Polish
	person := uuid.NewString()
	p.Narrator = &Narrator{PersonID: person, Name: "我"}
	p.Targets[0].Turns[0].PersonID = person
	check := func() []string {
		body, parameters := preparePolish(context.Background(), p, promptconfig.Style{}, nil, promptconfig.Parameters{MaxOutputTokens: 4096, TimeoutSeconds: 90})
		if parameters.TimeoutSeconds <= 30 || parameters.TimeoutSeconds >= 45 {
			t.Fatal("full organization must tolerate provider latency within the App's 45-second inactivity bound")
		}
		properties := body["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
		return properties["omissions"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["reason"].(map[string]any)["enum"].([]string)
	}
	if reasons := check(); slices.Contains(reasons, "recognitionNoise") || slices.Contains(reasons, "backgroundConversation") || !slices.Contains(reasons, "speechDisfluency") {
		t.Fatal("generation schema contradicts the author omission guard")
	}
	p.Targets[0].Turns[0].PersonID = ""
	if reasons := check(); !slices.Contains(reasons, "recognitionNoise") || !slices.Contains(reasons, "backgroundConversation") {
		t.Fatal("unknown background material lost its valid disposition choices")
	}
}

func TestFullAuthorDraftPreparesBoundedFactsBeforeProse(t *testing.T) {
	p := *comparisonSnapshot([]string{"今天去图书馆还书。"}, false).Polish
	parameters := promptconfig.Parameters{Model: "fixture", ReasoningEffort: "disabled", MaxOutputTokens: 12000, TimeoutSeconds: 90}
	for _, complete := range []bool{true, false} {
		p.Targets[0].CompleteSource = complete
		body, effective := preparePolish(context.Background(), p, promptconfig.Style{}, nil, parameters)
		properties := body["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
		_, hasChecks := properties["checks"]
		if hasChecks != complete || effective.TimeoutSeconds != 40 || effective.ReasoningEffort != "disabled" {
			t.Fatal("full-draft checks changed an incomplete or latency-sensitive request", effective)
		}
		wantTokens := 2048
		if complete {
			wantTokens = 4096
			if effective.Temperature == nil || *effective.Temperature != 0 {
				t.Fatal("full-draft default sampling is not stable")
			}
		}
		if effective.MaxOutputTokens != wantTokens {
			t.Fatal("fact scaffold and prose budget differ from the effective request", effective)
		}
	}
	p.Targets[0].CompleteSource = true
	configured := 0.6
	parameters.Temperature = &configured
	_, effective := preparePolish(context.Background(), p, promptconfig.Style{}, nil, parameters)
	if *effective.Temperature != configured {
		t.Fatal("explicit operator sampling was overwritten")
	}
}
