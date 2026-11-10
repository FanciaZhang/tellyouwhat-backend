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
