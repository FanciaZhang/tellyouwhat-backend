package adminportal

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type rateWindow struct {
	startedAt time.Time
	count     int
}

type rateLimiter struct {
	mutex   sync.Mutex
	windows map[string]rateWindow
	now     func() time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{windows: map[string]rateWindow{}, now: now}
}

func (limiter *rateLimiter) allow(request *http.Request) bool {
	if !strings.HasPrefix(request.URL.Path, "/api/") {
		return true
	}
	limit, bucket := 180, "api"
	if strings.HasPrefix(request.URL.Path, "/api/v1/auth/") || strings.HasPrefix(request.URL.Path, "/api/v1/security/") {
		limit, bucket = 30, "auth"
	}
	ip := strings.TrimSpace(request.Header.Get("X-Real-IP"))
	if net.ParseIP(ip) == nil {
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err == nil {
			ip = host
		} else {
			ip = request.RemoteAddr
		}
	}
	key := bucket + ":" + ip
	now := limiter.now()
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	window := limiter.windows[key]
	if window.startedAt.IsZero() || now.Sub(window.startedAt) >= time.Minute {
		window = rateWindow{startedAt: now}
	}
	window.count++
	limiter.windows[key] = window
	if len(limiter.windows) > 10000 {
		for oldKey, candidate := range limiter.windows {
			if now.Sub(candidate.startedAt) >= 2*time.Minute {
				delete(limiter.windows, oldKey)
			}
		}
	}
	return window.count <= limit
}
