package voice

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

type diagnosticFailureRewriter struct{}

func (diagnosticFailureRewriter) Rewrite(context.Context, Snapshot, int) (RewriteResult, error) {
	return RewriteResult{Diagnostics: RewriteDiagnostics{Stage: "fixture_failure", HTTPStatus: 503}}, errors.New("fixture")
}

func TestVoiceSessionLogsCorrelatedRewriteMetadataWithoutContent(t *testing.T) {
	const privateBody = "只属于用户的正文内容"
	const privateTranscript = "只属于用户的口述内容"
	var logs bytes.Buffer
	service := &Service{
		Store: NewMemoryStore(), Model: diagnosticFailureRewriter{}, Secret: make([]byte, 32),
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	session := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { service.Serve(w, r, session) }))
	defer server.Close()
	ticket, err := service.Issue(context.Background(), Identity{Owner: "owner", Anchor: time.Now().AddDate(0, -1, 0), ExpiresAt: time.Now().Add(time.Hour)}, session)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), "http://localhost")
	config.Header.Set("Authorization", "Bearer "+ticket.Token)
	ws, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetDeadline(time.Now().Add(5 * time.Second))
	var event Event
	if err = websocket.JSON.Receive(ws, &event); err != nil || event.Type != "ready" {
		t.Fatalf("ready=%+v err=%v", event, err)
	}
	snapshot := Snapshot{Revision: 4, Blocks: []Block{{ID: uuid.NewString(), Text: privateBody}}, Transcript: privateTranscript}
	if err = websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
		t.Fatal(err)
	}
	if err = websocket.JSON.Send(ws, Frame{Type: "finish"}); err != nil {
		t.Fatal(err)
	}
	if err = websocket.JSON.Receive(ws, &event); err != nil || event.Type != "error" || event.Code != "voice_rewrite_unavailable" {
		t.Fatalf("error event=%+v err=%v", event, err)
	}
	_ = ws.Close()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "journal voice stream closed") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	output := logs.String()
	for _, expected := range []string{"journal voice stream started", "journal voice rewrite started", "journal voice rewrite completed", "fixture_failure", `"document_revision":4`, `"transcript_revision":0`, `"finalizing":`, "voice_trace_id"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log missing %q: %s", expected, output)
		}
	}
	for _, private := range []string{privateBody, privateTranscript, ticket.Token, session, "owner"} {
		if strings.Contains(output, private) {
			t.Fatalf("log leaked private value %q: %s", private, output)
		}
	}
}
