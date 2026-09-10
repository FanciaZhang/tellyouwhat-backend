package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func command(c *Controller, action, id string) int {
	body, _ := json.Marshal(map[string]string{"bootID": id})
	r := httptest.NewRequest("POST", "/"+action, strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	c.Control().ServeHTTP(w, r)
	return w.Code
}
func TestStandbyPauseAndProcessIdentity(t *testing.T) {
	c, _ := New("blue")
	ctx := WithController(context.Background(), c)
	id := c.Status().BootID
	if _, ok := Begin(ctx); ok {
		t.Fatal("standby claimed work")
	}
	if got := command(c, "resume", id); got != 409 {
		t.Fatal(got)
	}
	if got := command(c, "serve", "previous-process"); got != 409 {
		t.Fatal(got)
	}
	command(c, "serve", id)
	command(c, "resume", id)
	done, ok := Begin(ctx)
	if !ok {
		t.Fatal("active refused work")
	}
	command(c, "pause", id)
	if _, ok := Begin(ctx); ok {
		t.Fatal("paused claimed new work")
	}
	if ctx.Err() != nil {
		t.Fatal("pause cancelled existing work")
	}
	if command(c, "seal", id) != 409 {
		t.Fatal("sealed active task")
	}
	done()
	done()
	if command(c, "seal", id) != 200 {
		t.Fatal("could not seal empty instance")
	}
	restarted, _ := New("blue")
	if command(restarted, "resume", id) != 409 || restarted.Status().BackgroundEnabled {
		t.Fatal("restart inherited authority")
	}
}
func TestHTTPAndWebSocketRemainCountedUntilCompletion(t *testing.T) {
	c, _ := New("green")
	id := c.Status().BootID
	entered := make(chan struct{})
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; fmt.Fprint(w, "done") })
	server := httptest.NewServer(c.Handler(next))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.Status)
	}
	command(c, "serve", id)
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		r, e := http.Get(server.URL)
		if e == nil {
			r.Body.Close()
		}
	}()
	<-entered
	command(c, "pause", id)
	if command(c, "seal", id) != 409 || c.Status().HTTP != 1 {
		t.Fatal("lost running HTTP request")
	}
	close(release)
	<-completed
	if command(c, "seal", id) != 200 {
		t.Fatal("HTTP did not drain")
	}

	command(c, "serve", id)
	socket := httptest.NewServer(c.Handler(websocket.Handler(func(ws *websocket.Conn) {
		var text string
		_ = websocket.Message.Receive(ws, &text)
		_ = websocket.Message.Send(ws, text)
	})))
	defer socket.Close()
	ws, err := websocket.Dial("ws"+strings.TrimPrefix(socket.URL, "http"), "", socket.URL)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status().HTTP != 1 || command(c, "seal", id) != 409 {
		t.Fatal("WebSocket was not counted")
	}
	_ = websocket.Message.Send(ws, "complete")
	var received string
	if err = websocket.Message.Receive(ws, &received); err != nil || received != "complete" {
		t.Fatalf("%s %v", received, err)
	}
	ws.Close()
	deadline := time.Now().Add(time.Second)
	for c.Status().HTTP != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if command(c, "seal", id) != 200 {
		t.Fatal("WebSocket did not drain")
	}
}
func TestStreamingFlushAndHijackerPreserved(t *testing.T) {
	c, _ := New("")
	release := make(chan struct{})
	server := httptest.NewServer(c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("hijacker hidden")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	})))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("%q %v", line, err)
	}
	if c.Status().HTTP != 1 {
		t.Fatal("stream not counted")
	}
	close(release)
	resp.Body.Close()
}
func TestPauseAdmissionRace(t *testing.T) {
	c, _ := New("")
	ctx := WithController(context.Background(), c)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if done, ok := Begin(ctx); ok {
				done()
			}
		}()
	}
	command(c, "pause", c.Status().BootID)
	wg.Wait()
	if c.Status().Background != 0 {
		t.Fatal("leaked reservation")
	}
	for range 100 {
		if _, ok := Begin(ctx); ok {
			t.Fatal("post-pause admission")
		}
	}
}
func TestListenerIsLoopbackOnly(t *testing.T) {
	for _, role := range []string{"gateway", "worker", "admin"} {
		addr, err := Address(role)
		if err != nil {
			t.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(addr)
		if host != "127.0.0.1" {
			t.Fatal(addr)
		}
	}
	if _, err := New("unexpected"); err == nil {
		t.Fatal("invalid slot accepted")
	}
}

func TestDetachedSettlementPreventsSealAfterResponse(t *testing.T) {
	c, _ := New("")
	var complete func()
	response := httptest.NewRecorder()
	c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		complete = Track(r.Context())
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(response, httptest.NewRequest("POST", "/work", nil))
	command(c, "pause", c.Status().BootID)
	if c.Status().HTTP != 0 || c.Status().Background != 1 || command(c, "seal", c.Status().BootID) != 409 {
		t.Fatal("response completion hid unfinished settlement")
	}
	complete()
	if command(c, "seal", c.Status().BootID) != 200 {
		t.Fatal("settlement did not drain")
	}
}
