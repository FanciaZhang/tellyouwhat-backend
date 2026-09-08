package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

// Used only by the explicitly enabled synthetic live test. This exercises the
// same consent, ticket, snapshot, revision and acknowledgement path as the App.
type developmentEditRewriter struct{ base string }

func (m developmentEditRewriter) Rewrite(ctx context.Context, snapshot Snapshot, _ int) (RewriteResult, error) {
	if !strings.HasPrefix(m.base, "https://") || !strings.HasSuffix(m.base, "/_development/journal") {
		return RewriteResult{}, errors.New("isolated development URL required")
	}
	token := os.Getenv("JOURNAL_DEVELOPMENT_TOKEN")
	if token == "" {
		return RewriteResult{}, errors.New("missing development credential")
	}
	installation := uuid.NewString()
	startedAt := time.Now().UTC().Format(time.RFC3339)
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, body any, want int, value any) error {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, "POST", m.base+path, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
		req.Header.Set("X-Journal-Development-Installation", installation)
		req.Header.Set("X-Journal-Development-Mode", "monthly")
		req.Header.Set("X-Journal-Development-Started-At", startedAt)
		response, err := client.Do(req)
		if err != nil {
			return errors.New("development request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			return fmt.Errorf("development %s returned HTTP %d", path, response.StatusCode)
		}
		if value != nil {
			return json.NewDecoder(response.Body).Decode(value)
		}
		return nil
	}
	if err := post("/v1/privacy/consents", map[string]any{"consents": []any{map[string]any{"scope": "managed_subscription", "documentVersion": "2026-08-24", "granted": true}}}, 200, nil); err != nil {
		return RewriteResult{}, err
	}
	session := uuid.NewString()
	var ticket Ticket
	if err := post("/v1/journal/voice/sessions", map[string]string{"sessionID": session, "consentVersion": Version}, 201, &ticket); err != nil {
		return RewriteResult{}, err
	}
	config, err := websocket.NewConfig("wss"+strings.TrimPrefix(m.base, "https")+"/v1/journal/voice/sessions/"+session+"/stream", "https://localhost")
	if err != nil {
		return RewriteResult{}, err
	}
	config.Header.Set("Authorization", "Bearer "+ticket.Token)
	ws, err := config.DialContext(ctx)
	if err != nil {
		return RewriteResult{}, errors.New("development websocket failed")
	}
	defer ws.Close()
	_ = ws.SetDeadline(time.Now().Add(60 * time.Second))
	var event Event
	if websocket.JSON.Receive(ws, &event) != nil || event.Type != "ready" {
		return RewriteResult{}, errors.New("development stream not ready")
	}
	if websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}) != nil || websocket.JSON.Send(ws, Frame{Type: "finish"}) != nil {
		return RewriteResult{}, errors.New("snapshot send failed")
	}
	var result RewriteResult
	seen := false
	for {
		if websocket.JSON.Receive(ws, &event) != nil {
			return RewriteResult{}, errors.New("development result unavailable")
		}
		switch event.Type {
		case "error":
			return RewriteResult{}, errors.New(event.Code)
		case "finished":
			if !seen {
				return RewriteResult{}, errors.New("no editorial result")
			}
			return result, nil
		case "revision":
			if seen || event.Revision == nil || event.Revision.Validate(snapshot) != nil {
				return RewriteResult{}, errors.New("invalid editorial result")
			}
			seen = true
			result.Revision = *event.Revision
			for _, p := range event.Revision.Patches {
				if p.AfterID != "" {
					return RewriteResult{}, errors.New("correction appended instead of replacing")
				}
				for i := range snapshot.Blocks {
					if snapshot.Blocks[i].ID == p.ID {
						snapshot.Blocks[i].Text = p.Text
					}
				}
			}
			snapshot.Revision++
			if websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}) != nil {
				return RewriteResult{}, errors.New("revision acknowledgement failed")
			}
		}
	}
}
