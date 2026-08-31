package appregistry

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func fixtureApps() []App {
	return []App{
		{ID: Health, DisplayName: "告你健康", Hosts: []string{"api.health.test"}, TeamID: "TEAM", BundleID: "health.bundle", ManagedAIProductID: "health.ai.monthly", AllowedOperationPrefix: "health."},
		{ID: Journal, DisplayName: "告你手记", Hosts: []string{"api.journal.test"}, TeamID: "TEAM", BundleID: "journal.bundle", ManagedAIProductID: "journal.ai.monthly", AllowedOperationPrefix: "journal."},
	}
}

func TestRegistryRejectsDuplicateHostAndCrossAppOperation(t *testing.T) {
	apps := fixtureApps()
	registry, err := New(apps)
	if err != nil {
		t.Fatal(err)
	}
	health, _ := registry.ResolveHost("API.HEALTH.TEST:443")
	if health.ID != Health || !health.AllowsOperation("health.meal.decision") || health.AllowsOperation("journal.organize") {
		t.Fatal("host or operation isolation failed")
	}
	apps[1].Hosts = []string{"api.health.test"}
	if _, err := New(apps); err == nil {
		t.Fatal("duplicate host was accepted")
	}
}

func TestHostMuxRejectsUnknownHostAndDispatchesKnownHost(t *testing.T) {
	registry, _ := New(fixtureApps())
	handlers := map[AppID]http.Handler{
		Health:  http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(204) }),
		Journal: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(202) }),
	}
	mux, err := NewHostMux(registry, handlers)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://api.journal.test/readyz", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != 202 {
		t.Fatalf("unexpected journal status %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "https://unknown.test/readyz", nil)
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unexpected unknown-host status %d", response.Code)
	}
}
