package recognitionquota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	DailySessionLimit = 3
	MaximumSessionAge = 2 * time.Hour
)

var (
	ErrExceeded = errors.New("recognition session quota exceeded")
	ErrInvalid  = errors.New("invalid recognition session")
	ErrNotFound = errors.New("recognition session not found")
)

type Context struct {
	SessionID            string
	BusinessDayStartHour int
	TimeZoneIdentifier   string
}

type Request struct {
	DeviceID string
	Context  Context
}

type WindowSettings struct {
	BusinessDayStartHour int
	TimeZoneIdentifier   string
}

type Snapshot struct {
	Completed int
	Reserved  int
	Remaining int
	ResetAt   time.Time
}

type Store interface {
	Reserve(context.Context, Request, time.Time) (Snapshot, error)
	Complete(context.Context, string, string, time.Time) (Snapshot, error)
	Cancel(context.Context, string, string, time.Time) error
	Snapshot(context.Context, string, WindowSettings, time.Time) (Snapshot, error)
}

type sessionState struct {
	completed bool
	expiresAt time.Time
}

type deviceWindow struct {
	start    time.Time
	end      time.Time
	sessions map[string]sessionState
}

type MemoryStore struct {
	mu      sync.Mutex
	windows map[string]deviceWindow
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{windows: make(map[string]deviceWindow)}
}

func (store *MemoryStore) Reserve(_ context.Context, request Request, now time.Time) (Snapshot, error) {
	if store == nil || request.DeviceID == "" || !validContext(request.Context) {
		return Snapshot{}, ErrInvalid
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	window, err := store.window(request.DeviceID, WindowSettings{
		BusinessDayStartHour: request.Context.BusinessDayStartHour,
		TimeZoneIdentifier:   request.Context.TimeZoneIdentifier,
	}, now, true)
	if err != nil {
		return Snapshot{}, err
	}
	cleanupExpired(&window, now)
	if _, ok := window.sessions[request.Context.SessionID]; ok {
		store.windows[request.DeviceID] = window
		return snapshot(window), nil
	}
	if sessionCount(window) >= DailySessionLimit {
		store.windows[request.DeviceID] = window
		return snapshot(window), ErrExceeded
	}
	expiresAt := now.Add(MaximumSessionAge)
	if expiresAt.After(window.end) {
		expiresAt = window.end
	}
	window.sessions[request.Context.SessionID] = sessionState{expiresAt: expiresAt}
	store.windows[request.DeviceID] = window
	return snapshot(window), nil
}

func (store *MemoryStore) Complete(_ context.Context, deviceID, sessionID string, now time.Time) (Snapshot, error) {
	if store == nil || deviceID == "" || uuid.Validate(sessionID) != nil {
		return Snapshot{}, ErrInvalid
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	window, ok := store.windows[deviceID]
	if !ok || !now.Before(window.end) {
		return Snapshot{}, ErrNotFound
	}
	cleanupExpired(&window, now)
	state, ok := window.sessions[sessionID]
	if !ok {
		store.windows[deviceID] = window
		return snapshot(window), ErrNotFound
	}
	state.completed = true
	state.expiresAt = window.end
	window.sessions[sessionID] = state
	store.windows[deviceID] = window
	return snapshot(window), nil
}

func (store *MemoryStore) Cancel(_ context.Context, deviceID, sessionID string, now time.Time) error {
	if store == nil || deviceID == "" || uuid.Validate(sessionID) != nil {
		return ErrInvalid
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	window, ok := store.windows[deviceID]
	if !ok || !now.Before(window.end) {
		return nil
	}
	cleanupExpired(&window, now)
	if state, exists := window.sessions[sessionID]; exists && !state.completed {
		delete(window.sessions, sessionID)
	}
	store.windows[deviceID] = window
	return nil
}

func (store *MemoryStore) Snapshot(_ context.Context, deviceID string, settings WindowSettings, now time.Time) (Snapshot, error) {
	if store == nil || deviceID == "" || !validSettings(settings) {
		return Snapshot{}, ErrInvalid
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	window, err := store.window(deviceID, settings, now, false)
	if err != nil {
		return Snapshot{}, err
	}
	cleanupExpired(&window, now)
	if existing, ok := store.windows[deviceID]; ok && existing.end.Equal(window.end) {
		store.windows[deviceID] = window
	}
	return snapshot(window), nil
}

func (store *MemoryStore) window(deviceID string, settings WindowSettings, now time.Time, lock bool) (deviceWindow, error) {
	if existing, ok := store.windows[deviceID]; ok && now.Before(existing.end) {
		return existing, nil
	}
	start, end, err := BusinessWindow(now, settings)
	if err != nil {
		return deviceWindow{}, err
	}
	window := deviceWindow{start: start, end: end, sessions: make(map[string]sessionState)}
	if lock {
		store.windows[deviceID] = window
	}
	return window, nil
}

func BusinessWindow(now time.Time, settings WindowSettings) (time.Time, time.Time, error) {
	if !validSettings(settings) {
		return time.Time{}, time.Time{}, ErrInvalid
	}
	location, err := time.LoadLocation(settings.TimeZoneIdentifier)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: time zone", ErrInvalid)
	}
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), settings.BusinessDayStartHour, 0, 0, 0, location)
	if local.Before(start) {
		previous := local.AddDate(0, 0, -1)
		start = time.Date(previous.Year(), previous.Month(), previous.Day(), settings.BusinessDayStartHour, 0, 0, 0, location)
	}
	next := start.In(location).AddDate(0, 0, 1)
	end := time.Date(next.Year(), next.Month(), next.Day(), settings.BusinessDayStartHour, 0, 0, 0, location)
	return start.UTC(), end.UTC(), nil
}

func validContext(value Context) bool {
	return uuid.Validate(value.SessionID) == nil && validSettings(WindowSettings{
		BusinessDayStartHour: value.BusinessDayStartHour,
		TimeZoneIdentifier:   value.TimeZoneIdentifier,
	})
}

func validSettings(value WindowSettings) bool {
	return value.BusinessDayStartHour >= 0 && value.BusinessDayStartHour <= 23 && value.TimeZoneIdentifier != ""
}

func cleanupExpired(window *deviceWindow, now time.Time) {
	for id, state := range window.sessions {
		if !state.completed && !now.Before(state.expiresAt) {
			delete(window.sessions, id)
		}
	}
}

func sessionCount(window deviceWindow) int {
	return len(window.sessions)
}

func snapshot(window deviceWindow) Snapshot {
	value := Snapshot{ResetAt: window.end}
	for _, state := range window.sessions {
		if state.completed {
			value.Completed++
		} else {
			value.Reserved++
		}
	}
	value.Remaining = DailySessionLimit - value.Completed - value.Reserved
	if value.Remaining < 0 {
		value.Remaining = 0
	}
	return value
}
