package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/tellyouwhat/backend/internal/promptconfig"
	"golang.org/x/net/websocket"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestASRConnectionFreezesPublishedParameters(t *testing.T) {
	requests := make(chan map[string]any, 2)
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var packet []byte
		if err := websocket.Message.Receive(ws, &packet); err != nil {
			return
		}
		if len(packet) < 8 {
			return
		}
		length := int(binary.BigEndian.Uint32(packet[4:8]))
		raw := packet[8 : 8+length]
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		requests <- value
		var finish []byte
		_ = websocket.Message.Receive(ws, &finish)
	}))
	defer server.Close()
	client := ASR{Config: ASRConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), APIKey: "test", ResourceID: "test"}}
	p := promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]
	p.Journal.Voice.AutomaticPunctuation = false
	p.Journal.Voice.NormalizeNumbers = false
	first := promptconfig.WithRevision(context.Background(), promptconfig.Revision{ID: "first", Scope: "journal", Policy: p})
	connection, err := client.Open(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	select {
	case request = <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("missing ASR request")
	}
	_ = connection.Close()
	settings := request["request"].(map[string]any)
	if settings["enable_punc"] != false || settings["enable_itn"] != false {
		t.Fatal("frozen parameters missing", settings)
	}
	p.Journal.Voice.AutomaticPunctuation = true
	p.Journal.Voice.NormalizeNumbers = true
	next := promptconfig.WithRevision(context.Background(), promptconfig.Revision{ID: "next", Scope: "journal", Policy: p})
	connection, err = client.Open(next, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request = <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("missing next ASR request")
	}
	_ = connection.Close()
	settings = request["request"].(map[string]any)
	if settings["enable_punc"] != true || settings["enable_itn"] != true {
		t.Fatal("new connection did not use new snapshot", settings)
	}
}
