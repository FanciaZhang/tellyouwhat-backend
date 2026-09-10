package lifecycle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// CI supplies the production Caddy version. No provider, database or production
// endpoint is called: these are real TCP streams through an isolated proxy.
func TestCaddyReloadPreservesRequestsAndStreams(t *testing.T) {
	binary := os.Getenv("CADDY_TEST_BIN")
	if binary == "" {
		t.Skip("CADDY_TEST_BIN is required for real proxy acceptance")
	}
	available := func() string {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	}
	admin, public := available(), available()
	release := make(chan struct{})
	slowEntered := make(chan struct{})
	old, _ := New("blue")
	newer, _ := New("green")
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "old") })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(slowEntered)
		<-release
		io.WriteString(w, "old finished")
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: start\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: finish\n\n")
	})
	mux.Handle("/voice", websocket.Handler(func(ws *websocket.Conn) {
		for {
			var text string
			if websocket.Message.Receive(ws, &text) != nil {
				return
			}
			_ = websocket.Message.Send(ws, text)
			if text == "finish" {
				return
			}
		}
	}))
	a := httptest.NewServer(old.Handler(mux))
	defer a.Close()
	b := httptest.NewServer(newer.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new") })))
	defer b.Close()
	var releaseOnce sync.Once
	finishRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer finishRelease()
	command(old, "serve", old.Status().BootID)
	command(old, "resume", old.Status().BootID)
	config := func(upstream string) []byte {
		value := map[string]any{"admin": map[string]any{"listen": admin}, "apps": map[string]any{"http": map[string]any{"servers": map[string]any{"acceptance": map[string]any{
			"listen": []string{public}, "routes": []any{map[string]any{"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]string{"dial": strings.TrimPrefix(upstream, "http://")}}, "stream_close_delay": int64(4 * time.Hour), "flush_interval": -1}}}},
		}}}}}
		data, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return data
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "caddy.json")
	os.WriteFile(path, config(a.URL), 0600)
	logFile, e := os.Create(filepath.Join(directory, "caddy.log"))
	if e != nil {
		t.Fatal(e)
	}
	defer logFile.Close()
	process := exec.Command(binary, "run", "--config", path)
	process.Env = append(os.Environ(), "XDG_DATA_HOME="+directory, "XDG_CONFIG_HOME="+directory)
	process.Stdout = logFile
	process.Stderr = logFile
	if e = process.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		process.Process.Kill()
		process.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logFile.Name())
			t.Log(string(data))
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://" + public
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, e := client.Get(base + "/version")
		if e == nil {
			r.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stream, e := client.Get(base + "/events")
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	if line, e := reader.ReadString('\n'); e != nil || line != "data: start\n" {
		t.Fatalf("stream start %q %v", line, e)
	}
	ws, e := websocket.Dial("ws://"+public+"/voice", "", base)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	slowDone := make(chan error, 1)
	go func() {
		r, e := client.Get(base + "/slow")
		if e == nil {
			_, e = io.ReadAll(r.Body)
			r.Body.Close()
		}
		slowDone <- e
	}()
	<-slowEntered
	var failed, probes atomic.Int64
	stop := make(chan struct{})
	probeDone := make(chan struct{})
	var stopOnce sync.Once
	stopProbes := func() { stopOnce.Do(func() { close(stop) }); <-probeDone }
	defer stopProbes()
	go func() {
		defer close(probeDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r, e := client.Get(base + "/version")
			if e != nil {
				failed.Add(1)
			} else {
				if r.StatusCode != 200 {
					failed.Add(1)
				}
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
			}
			probes.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	waitProbes := func(minimum int64) {
		deadline := time.Now().Add(5 * time.Second)
		for probes.Load() < minimum {
			if time.Now().After(deadline) {
				t.Fatal("continuous probe deadline exceeded")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitProbes(100)
	reload := func(target string) {
		r, e := client.Post("http://"+admin+"/load", "application/json", bytes.NewReader(config(target)))
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			body, _ := io.ReadAll(r.Body)
			t.Fatalf("reload %s %s", r.Status, body)
		}
	}
	command(old, "pause", old.Status().BootID)
	command(newer, "serve", newer.Status().BootID)
	reload(b.URL)
	command(newer, "resume", newer.Status().BootID)
	waitProbes(200)
	r, e := client.Get(base + "/version")
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(body) != "new" {
		t.Fatal("new traffic did not switch")
	}
	if command(old, "seal", old.Status().BootID) != 409 {
		t.Fatal("old streams allowed premature stop")
	}
	ws.SetDeadline(time.Now().Add(5 * time.Second))
	if e = websocket.Message.Send(ws, "after reload"); e != nil {
		t.Fatal(e)
	}
	var echo string
	if e = websocket.Message.Receive(ws, &echo); e != nil || echo != "after reload" {
		t.Fatalf("WebSocket interrupted: %v", e)
	}
	finishRelease()
	if e = <-slowDone; e != nil {
		t.Fatal(e)
	}
	remaining, e := io.ReadAll(reader)
	if e != nil || !strings.Contains(string(remaining), "data: finish") {
		t.Fatalf("SSE interrupted: %v %s", e, remaining)
	}
	websocket.Message.Send(ws, "finish")
	websocket.Message.Receive(ws, &echo)
	ws.Close()
	deadline = time.Now().Add(time.Second)
	for old.Status().HTTP > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if command(old, "seal", old.Status().BootID) != 200 {
		t.Fatal("old instance did not drain")
	}
	// Explicit reverse switch also retains availability.
	command(newer, "pause", newer.Status().BootID)
	command(old, "serve", old.Status().BootID)
	reload(a.URL)
	command(old, "resume", old.Status().BootID)
	r, e = client.Get(base + "/version")
	if e != nil {
		t.Fatal(e)
	}
	body, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if string(body) != "old" {
		t.Fatal("rollback did not restore old version")
	}
	waitProbes(300)
	stopProbes()
	if probes.Load() == 0 || failed.Load() != 0 {
		t.Fatalf("continuous probes: %d total, %d failures", probes.Load(), failed.Load())
	}
	t.Logf("continuous probes across forward and reverse reload: %d, failures: %d", probes.Load(), failed.Load())
}
