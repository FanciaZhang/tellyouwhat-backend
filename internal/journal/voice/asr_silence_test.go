package voice

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func silenceASRConnection(t *testing.T, responds bool) *asrConnection {
	t.Helper()
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		for {
			var packet []byte
			if websocket.Message.Receive(ws, &packet) != nil {
				return
			}
			if responds && len(packet) > 1 && packet[1]&2 != 0 {
				_ = websocket.Message.Send(ws, asrPacket(9, true, []byte(`{"result":{"text":"安静后继续记录。"}}`)))
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	connection, err := (ASR{Config: ASRConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http")}}).Open(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	c := connection.(*asrConnection)
	c.readTimeout = 200 * time.Millisecond
	return c
}

func TestASRSilentAudioKeepsItsActiveReceiveAlive(t *testing.T) {
	c := silenceASRConnection(t, true)
	result := make(chan error, 1)
	go func() {
		transcript, err := c.Receive()
		if err == nil && (!transcript.Final || transcript.Text != "安静后继续记录。") {
			err = ErrInvalid
		}
		result <- err
	}()
	for range 8 {
		time.Sleep(50 * time.Millisecond)
		if err := c.Send(make([]byte, 1600), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Send(nil, true); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal("valid quiet audio was mistaken for a broken provider", err)
	}
}

func TestASRStillTimesOutWhenInputAndProviderBothStop(t *testing.T) {
	c := silenceASRConnection(t, false)
	_, err := c.Receive()
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal("a stalled connection must retain its bounded timeout", err)
	}
}
