package voice

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

// Explicit deployment acceptance, using invented journal text only. The local
// credential is read into memory and never logged or included in provider input.
func TestLiveWritingStylesOnDevelopmentService(t *testing.T) {
	if os.Getenv("JOURNAL_WRITING_STYLE_LIVE_CHECK") != "1" {
		t.Skip("explicit live development acceptance only")
	}
	base := os.Getenv("JOURNAL_WRITING_STYLE_LIVE_URL")
	if !strings.HasPrefix(base, "https://") || !strings.HasSuffix(base, "/_development/journal") {
		t.Fatal("provide the isolated development URL")
	}
	data, err := os.ReadFile(os.Getenv("JOURNAL_WRITING_STYLE_SESSION_FILE"))
	if err != nil {
		t.Fatal("development session file unavailable")
	}
	var credential struct{ Token string }
	if json.Unmarshal(data, &credential) != nil || credential.Token == "" {
		t.Fatal("development credential unavailable")
	}
	for _, style := range []WritingStyle{StyleNatural, StyleLively, StyleDocumentary, StyleDaybook, StyleEssay} {
		t.Run(string(style), func(t *testing.T) {
			installation := uuid.NewString()
			post := func(path string, body any, status int) []byte {
				raw, _ := json.Marshal(body)
				req, _ := http.NewRequest("POST", base+path, bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer "+credential.Token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Tellyouwhat-Request-ID", uuid.NewString())
				req.Header.Set("X-Journal-Development-Installation", installation)
				req.Header.Set("X-Journal-Development-Mode", "monthly")
				req.Header.Set("X-Journal-Development-Started-At", time.Now().UTC().Format(time.RFC3339))
				response, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
				if err != nil {
					t.Fatal("development HTTP request failed")
				}
				defer response.Body.Close()
				if response.StatusCode != status {
					t.Fatalf("development %s returned %d", path, response.StatusCode)
				}
				raw, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
				if err != nil {
					t.Fatal("response unreadable")
				}
				return raw
			}
			post("/v1/privacy/consents", map[string]any{"consents": []any{map[string]any{"scope": "managed_subscription", "documentVersion": "2026-08-24", "granted": true}}}, 200)
			session := uuid.NewString()
			raw := post("/v1/journal/voice/sessions", map[string]string{"sessionID": session, "consentVersion": Version}, 201)
			var ticket Ticket
			if json.Unmarshal(raw, &ticket) != nil || ticket.Token == "" {
				t.Fatal("missing voice ticket")
			}
			config, _ := websocket.NewConfig("wss"+strings.TrimPrefix(base, "https")+"/v1/journal/voice/sessions/"+session+"/stream", "https://localhost")
			config.Header.Set("Authorization", "Bearer "+ticket.Token)
			ws, err := websocket.DialConfig(config)
			if err != nil {
				t.Fatal("development WebSocket failed")
			}
			defer ws.Close()
			_ = ws.SetDeadline(time.Now().Add(75 * time.Second))
			snapshot := Snapshot{WritingStyle: style, Blocks: []Block{{uuid.NewString(), "早上八点，我出门上班。"}, {uuid.NewString(), "晚上八点，我回到家。"}},
				Transcript: "补充一下，下午三点我去河边走了走，买了杯热茶，坐了一会儿，心情轻松了些。对了，中午十二点还在公司附近吃了面。不是牛肉面，是番茄鸡蛋面。"}
			snapshot.EditedBlockIDs = []string{snapshot.Blocks[0].ID, snapshot.Blocks[1].ID}
			send := func(frame Frame) {
				if websocket.JSON.Send(ws, frame) != nil {
					t.Fatal("development send failed")
				}
			}
			var event Event
			if websocket.JSON.Receive(ws, &event) != nil || event.Type != "ready" {
				t.Fatal("development stream not ready")
			}
			send(Frame{Type: "snapshot", Snapshot: &snapshot})
			send(Frame{Type: "finish"})
			revisions := 0
			for {
				if websocket.JSON.Receive(ws, &event) != nil {
					t.Fatal("development result unavailable")
				}
				if event.Type == "error" {
					t.Fatalf("development rejected: %s", event.Code)
				}
				if event.Type == "finished" {
					break
				}
				if event.Type != "revision" {
					continue
				}
				if event.Revision == nil || event.Revision.Validate(snapshot) != nil {
					t.Fatal("invalid or protected-block rewrite")
				}
				revisions++
				for _, patch := range event.Revision.Patches {
					if patch.AfterID == "" {
						for i := range snapshot.Blocks {
							if snapshot.Blocks[i].ID == patch.ID {
								snapshot.Blocks[i].Text = patch.Text
							}
						}
					} else {
						for i := range snapshot.Blocks {
							if snapshot.Blocks[i].ID == patch.AfterID {
								snapshot.Blocks = append(snapshot.Blocks[:i+1], append([]Block{{patch.ID, patch.Text}}, snapshot.Blocks[i+1:]...)...)
								break
							}
						}
					}
				}
				snapshot.Revision++
				send(Frame{Type: "snapshot", Snapshot: &snapshot})
			}
			var paragraphs []string
			for _, block := range snapshot.Blocks {
				paragraphs = append(paragraphs, block.Text)
			}
			body := strings.Join(paragraphs, "\n")
			if revisions == 0 || !strings.Contains(body, "番茄鸡蛋面") || !strings.Contains(body, "河边") {
				t.Fatal("synthetic content was omitted")
			}
			if strings.Index(body, "番茄鸡蛋面") > strings.Index(body, "河边") {
				t.Fatal("late supplement was not put in chronological order")
			}
			t.Logf("Synthetic result (%s):\n%s", style, body)
		})
	}
}
