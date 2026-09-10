// Package lifecycle coordinates single-host release admission and work draining.
package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type key struct{}
type Status struct {
	Protocol          int    `json:"protocol"`
	BootID            string `json:"bootID"`
	Slot              string `json:"slot"`
	HTTPEnabled       bool   `json:"httpEnabled"`
	BackgroundEnabled bool   `json:"backgroundEnabled"`
	HTTP              int    `json:"http"`
	Background        int    `json:"background"`
}

type Controller struct {
	mu    sync.Mutex
	state Status
}

func New(slot string) (*Controller, error) {
	if slot != "" && slot != "blue" && slot != "green" {
		return nil, errors.New("invalid deployment slot")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return &Controller{state: Status{Protocol: 1, BootID: hex.EncodeToString(id[:]), Slot: slot, HTTPEnabled: slot == "", BackgroundEnabled: slot == ""}}, nil
}
func WithController(ctx context.Context, c *Controller) context.Context {
	return context.WithValue(ctx, key{}, c)
}
func (c *Controller) Status() Status { c.mu.Lock(); defer c.mu.Unlock(); return c.state }

// Begin registers the entire acquisition/execution/settlement interval before a
// background claim. Pause and Begin share a mutex, so an acknowledged pause
// cannot race with an unregistered claim. Existing work is never cancelled.
func Begin(ctx context.Context) (func(), bool) {
	c, _ := ctx.Value(key{}).(*Controller)
	if c == nil {
		return func() {}, ctx.Err() == nil
	}
	c.mu.Lock()
	if !c.state.BackgroundEnabled || ctx.Err() != nil {
		c.mu.Unlock()
		return nil, false
	}
	c.state.Background++
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.state.Background--; c.mu.Unlock() }) }, true
}
func Wait(ctx context.Context) bool {
	for {
		c, _ := ctx.Value(key{}).(*Controller)
		if ctx.Err() != nil {
			return false
		}
		if c == nil || c.Status().BackgroundEnabled {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}
func (c *Controller) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Readiness remains available for candidates; it does not authorize traffic.
		if r.Method == http.MethodGet && (r.URL.Path == "/readyz" || r.URL.Path == "/healthz") {
			next.ServeHTTP(w, r)
			return
		}
		c.mu.Lock()
		if !c.state.HTTPEnabled {
			c.mu.Unlock()
			http.Error(w, "service is in standby", http.StatusServiceUnavailable)
			return
		}
		c.state.HTTP++
		c.mu.Unlock()
		defer func() { c.mu.Lock(); c.state.HTTP--; c.mu.Unlock() }()
		// Do not wrap ResponseWriter: streaming, flushing and hijacking stay native.
		next.ServeHTTP(w, r)
	})
}
func (c *Controller) Control() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(c.Status())
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			BootID string `json:"bootID"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if req.BootID != c.state.BootID {
			http.Error(w, "process changed", 409)
			return
		}
		switch r.URL.Path {
		case "/serve":
			c.state.HTTPEnabled = true
		case "/resume":
			if !c.state.HTTPEnabled {
				http.Error(w, "HTTP admission is disabled", 409)
				return
			}
			c.state.BackgroundEnabled = true
		case "/pause":
			c.state.BackgroundEnabled = false
		case "/seal":
			if c.state.BackgroundEnabled || c.state.HTTP != 0 || c.state.Background != 0 {
				http.Error(w, "work is still active", 409)
				return
			}
			c.state.HTTPEnabled = false
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.state)
	})
}
func Address(role string) (string, error) {
	switch role {
	case "gateway":
		return "127.0.0.1:19090", nil
	case "worker":
		return "127.0.0.1:19091", nil
	case "admin":
		return "127.0.0.1:19092", nil
	}
	return "", errors.New("invalid service role")
}
func (c *Controller) Listen(role string) (func(), error) {
	addr, err := Address(role)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: c.Control(), ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}

// Request only talks to the service's loopback control listener. The caller
// supplies the observed process ID so restarts cannot inherit old authority.
func Request(role, action, bootID string) (Status, error) {
	var status Status
	addr, err := Address(role)
	if err != nil {
		return status, err
	}
	if action != "status" && action != "serve" && action != "resume" && action != "pause" && action != "seal" {
		return status, errors.New("invalid lifecycle action")
	}
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
	}
	body, _ := json.Marshal(map[string]string{"bootID": bootID})
	req, err := http.NewRequest(method, "http://"+addr+"/"+action, strings.NewReader(string(body)))
	if err != nil {
		return status, err
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return status, errors.New("lifecycle listener unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return status, errors.New("lifecycle action rejected")
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&status)
	return status, err
}
