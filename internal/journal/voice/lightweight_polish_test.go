package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"golang.org/x/net/websocket"
)

func TestLightweightPolishUsesTrustedLiteRouteAndBoundedOutput(t *testing.T) {
	s := polishFixture()
	s.Polish.Lightweight = true
	s.IllustrationSuggestionsEnabled = true
	ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("approved-lite", "approved-pro", "approved-voice", 90)["journal"]})
	prepared, err := PrepareRewrite(ctx, s, 1, "untrusted-fallback")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if json.Unmarshal(prepared.Body, &body) != nil || body["model"] != "approved-lite" || prepared.Parameters.ReasoningEffort != "disabled" || prepared.TimeoutSeconds > 20 || prepared.Parameters.MaxOutputTokens > 2048 {
		t.Fatal("lightweight cleanup did not use its bounded trusted route")
	}
	format := body["text"].(map[string]any)["format"].(map[string]any)
	properties := format["schema"].(map[string]any)["properties"].(map[string]any)
	if properties["illustrationSuggestion"].(map[string]any)["type"] != "null" {
		t.Fatal("live cleanup must not generate illustration suggestions")
	}
}

func TestPolishSourceOffsetsAreBoundedAndFollowTheServerOwnedBaseline(t *testing.T) {
	s := comparisonSnapshot([]string{"第二个片段还没付款。"}, false)
	p := s.Polish
	p.Targets[0].SourceOffsets = []int{300}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Targets[0].SourceOffsets = []int{-1}
	if p.Validate() == nil {
		t.Fatal("negative source offset accepted")
	}
	p.Targets[0].SourceOffsets = []int{300, 600}
	if p.Validate() == nil {
		t.Fatal("offsets not aligned with source turns")
	}
}

type deferredPolishModel struct{ entered, release chan struct{} }

func (m deferredPolishModel) Rewrite(ctx context.Context, s Snapshot, _ int) (RewriteResult, error) {
	close(m.entered)
	select {
	case <-m.release:
	case <-ctx.Done():
		return RewriteResult{}, ctx.Err()
	}
	return RewriteResult{Polish: &PolishRevision{Targets: s.Polish.Targets, Paragraphs: []PolishParagraph{{Text: "今天去了河边。", Style: "body", TargetIDs: []string{s.Polish.Targets[0].ID}}}, Questions: []string{}}}, nil
}

func TestDeferredPolishWaitsWithoutCancellingAnAlreadyPaidRequest(t *testing.T) {
	m := deferredPolishModel{entered: make(chan struct{}), release: make(chan struct{})}
	service := &Service{Store: NewMemoryStore(), Model: m, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: "synthetic-deferred", Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	snapshot := polishFixture()
	snapshot.Polish.Deferred = true
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	voiceLifecycleUntil(t, ws, "pong")
	select {
	case <-m.entered:
		t.Fatal("deferred work started during continuous speech")
	default:
	}
	snapshot.Polish.Deferred = false
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	select {
	case <-m.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("a quiet boundary did not release the waiting work")
	}
	snapshot.Polish.Deferred = true
	websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
	websocket.JSON.Send(ws, Frame{Type: "ping"})
	voiceLifecycleUntil(t, ws, "pong")
	close(m.release)
	if event := voiceLifecycleUntil(t, ws, "polish"); event.Polish == nil {
		t.Fatal("new speech cancelled previously admitted work")
	}
}

func TestLightweightPolishNeverUsesNarrativeOmissionsToRemoveFacts(t *testing.T) {
	snapshot := comparisonSnapshot([]string{"预算280元，还没付款。"}, false)
	request := *snapshot.Polish
	request.Lightweight = true
	target := request.Targets[0]
	valid, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: target.SourceText, TargetIDs: []string{target.ID}, Style: "body"}}, "questions": []string{}, "omissions": []PolishOmission{}, "illustrationSuggestion": nil})
	revision, err := decodePolish(string(valid), request)
	if err != nil || !revision.Lightweight {
		t.Fatal("server did not preserve the chosen cleanup phase")
	}
	for _, reason := range []string{"backgroundConversation", "situationalInterruption", "speechDisfluency"} {
		invalid, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{}, "questions": []string{}, "omissions": []PolishOmission{{SourceID: target.Turns[0].ID, Text: target.SourceText, Reason: reason}}, "illustrationSuggestion": nil})
		if _, err := decodePolish(string(invalid), request); err == nil {
			t.Fatal("lightweight cleanup discarded a substantive source")
		}
	}
}

func TestLightweightPolishKeepsSourceWhenASRLeavesPartialWordNotes(t *testing.T) {
	snapshot := comparisonSnapshot([]string{"今天去河边看柳树。回。"}, false)
	request := *snapshot.Polish
	request.Lightweight = true
	target := request.Targets[0]
	response, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: "今天去河边看柳树。", TargetIDs: []string{target.ID}, Style: "body"}}, "questions": []string{}, "omissions": []PolishOmission{{SourceID: target.Turns[0].ID, Text: "回。", Reason: "speechDisfluency"}}, "illustrationSuggestion": nil})
	revision, err := decodePolish(string(response), request)
	if err != nil || len(revision.Omissions) != 0 {
		t.Fatal("partial-word notes must not discard or invalidate the whole source", err)
	}
}

func TestLightweightDiagnosticsReportTheChosenModelInsteadOfTheFullFallback(t *testing.T) {
	s := comparisonSnapshot([]string{"今天去河边看柳树。"}, false)
	s.Polish.Lightweight = true
	target := s.Polish.Targets[0]
	text, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: target.SourceText, TargetIDs: []string{target.ID}, Style: "body"}}, "questions": []string{}, "omissions": []PolishOmission{}, "illustrationSuggestion": nil})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "observed-lite", "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": string(text)}}}}})
	}))
	defer server.Close()
	ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("chosen-lite", "pro", "full-fallback", 90)["journal"]})
	result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "synthetic", Model: "full-fallback"}).Rewrite(ctx, s, 1)
	if err != nil || result.Diagnostics.RequestedModel != "chosen-lite" {
		t.Fatal("requested-model metadata described the wrong route", err)
	}
}

func TestRejectedLightweightOutputPreservesVisibleTextAndDefersFullOrganization(t *testing.T) {
	for _, sliced := range []bool{false, true} {
		snapshot := comparisonSnapshot([]string{"预算280元，还没付款。"}, false)
		snapshot.Polish.Lightweight = true
		target := &snapshot.Polish.Targets[0]
		if sliced {
			target.SourceOffsets = []int{300}
			target.Text = target.SourceText + "明天再确认。"
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "lite", "usage": map[string]int{"input_tokens": 10, "output_tokens": 10}, "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": `{"paragraphs":[],"omissions":[],"questions":[],"illustrationSuggestion":null}`}}}}})
		}))
		ctx := promptconfig.WithRevision(t.Context(), promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "pro", 90)["journal"]})
		result, err := (ArkRewriter{BaseURL: server.URL, APIKey: "synthetic", Model: "pro"}).Rewrite(ctx, snapshot, 1)
		server.Close()
		if err != nil || result.Polish == nil || !result.Polish.RequiresFullOrganization || !result.Polish.Lightweight || result.Diagnostics.Stage != "lightweight_preserved_source" {
			t.Fatal("invalid live cleanup blocked preserved speech", err)
		}
		if len(result.Polish.Paragraphs) != 1 || result.Polish.Paragraphs[0].Text != target.SourceText {
			t.Fatal("source preservation duplicated or dropped the unconsumed tail")
		}
		if !sliced {
			snapshot.Polish.Lightweight = false
			if _, err := decodePolish(result.OutputText, *snapshot.Polish); err == nil {
				t.Fatal("full organization must still reject missing facts")
			}
		}
	}
}
