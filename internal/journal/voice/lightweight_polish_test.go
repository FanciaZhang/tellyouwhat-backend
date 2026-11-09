package voice

import (
	"context"
	"encoding/json"
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
