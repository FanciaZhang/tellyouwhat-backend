package appregistry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

type AppID string

const (
	Health  AppID = "health"
	Journal AppID = "journal"
)

type PromptMode string

const (
	PromptClientComposed PromptMode = "client_composed"
	PromptServerTemplate PromptMode = "server_template"
	PromptHybrid         PromptMode = "hybrid"
)

type App struct {
	ID                     AppID
	DisplayName            string
	Hosts                  []string
	TeamID                 string
	BundleID               string
	AppAppleID             int64
	ManagedAIProductID     string
	AllowedOperationPrefix string
	AllowedOperations      []string
}

func (app App) Validate() error {
	if app.ID == "" || strings.TrimSpace(app.DisplayName) == "" || len(app.Hosts) == 0 ||
		strings.TrimSpace(app.TeamID) == "" || strings.TrimSpace(app.BundleID) == "" ||
		strings.TrimSpace(app.ManagedAIProductID) == "" ||
		(strings.TrimSpace(app.AllowedOperationPrefix) == "" && len(app.AllowedOperations) == 0) {
		return errors.New("app registry entry is incomplete")
	}
	if app.AllowedOperationPrefix != "" && !strings.HasSuffix(app.AllowedOperationPrefix, ".") {
		return errors.New("operation prefix must end in a dot")
	}
	seenOperations := make(map[string]struct{}, len(app.AllowedOperations))
	for _, operation := range app.AllowedOperations {
		operation = strings.TrimSpace(operation)
		if operation == "" {
			return errors.New("allowed operation is invalid")
		}
		if _, duplicate := seenOperations[operation]; duplicate {
			return errors.New("allowed operations contain a duplicate")
		}
		seenOperations[operation] = struct{}{}
	}
	for _, host := range app.Hosts {
		if normalizeHost(host) == "" {
			return errors.New("app registry host is invalid")
		}
	}
	return nil
}

func (app App) AllowsOperation(operation string) bool {
	for _, allowed := range app.AllowedOperations {
		if operation == allowed {
			return true
		}
	}
	return app.AllowedOperationPrefix != "" &&
		strings.HasPrefix(operation, app.AllowedOperationPrefix) &&
		len(operation) > len(app.AllowedOperationPrefix)
}

type Registry struct {
	byID   map[AppID]App
	byHost map[string]App
}

func New(apps []App) (*Registry, error) {
	registry := &Registry{byID: make(map[AppID]App), byHost: make(map[string]App)}
	for _, app := range apps {
		if err := app.Validate(); err != nil {
			return nil, fmt.Errorf("app %q: %w", app.ID, err)
		}
		if _, exists := registry.byID[app.ID]; exists {
			return nil, fmt.Errorf("duplicate app id %q", app.ID)
		}
		registry.byID[app.ID] = app
		for _, value := range app.Hosts {
			host := normalizeHost(value)
			if _, exists := registry.byHost[host]; exists {
				return nil, fmt.Errorf("duplicate app host %q", host)
			}
			registry.byHost[host] = app
		}
	}
	if len(registry.byID) == 0 {
		return nil, errors.New("app registry must contain at least one app")
	}
	return registry, nil
}

func (registry *Registry) App(id AppID) (App, bool) {
	if registry == nil {
		return App{}, false
	}
	app, ok := registry.byID[id]
	return app, ok
}

func (registry *Registry) ResolveHost(host string) (App, bool) {
	if registry == nil {
		return App{}, false
	}
	app, ok := registry.byHost[normalizeHost(host)]
	return app, ok
}

func (registry *Registry) Apps() []App {
	values := make([]App, 0, len(registry.byID))
	for _, app := range registry.byID {
		values = append(values, app)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values
}

func normalizeHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.TrimSuffix(value, ".")
}

type HostMux struct {
	registry *Registry
	routers  map[AppID]*gin.Engine
}

func NewHostMux(registry *Registry, routers map[AppID]*gin.Engine) (*HostMux, error) {
	if registry == nil {
		return nil, errors.New("app registry is required")
	}
	for _, app := range registry.Apps() {
		if routers[app.ID] == nil {
			return nil, fmt.Errorf("Gin router for app %q is required", app.ID)
		}
	}
	return &HostMux{registry: registry, routers: routers}, nil
}

func (mux *HostMux) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	app, ok := mux.registry.ResolveHost(request.Host)
	if !ok {
		http.Error(writer, "unknown application host", http.StatusMisdirectedRequest)
		return
	}
	mux.routers[app.ID].ServeHTTP(writer, request)
}
