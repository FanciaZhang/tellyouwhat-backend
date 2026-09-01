package appregistry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func fixtureApps() []App {
	return []App{
		{ID: Health, DisplayName: "告你健康", Hosts: []string{"api.health.test"}, TeamID: "TEAM", BundleID: "health.bundle", ManagedAIProductID: "health.ai.monthly", AllowedOperations: []string{"meal_decision", "meal_photo_capture"}},
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
	if health.ID != Health || !health.AllowsOperation("meal_decision") || health.AllowsOperation("meal") || health.AllowsOperation("journal.organize") {
		t.Fatal("host or operation isolation failed")
	}
	journal, _ := registry.ResolveHost("api.journal.test")
	if !journal.AllowsOperation("journal.organize") || journal.AllowsOperation("journal.") || journal.AllowsOperation("meal_decision") {
		t.Fatal("journal operation-prefix isolation failed")
	}
	apps[1].Hosts = []string{"api.health.test"}
	if _, err := New(apps); err == nil {
		t.Fatal("duplicate host was accepted")
	}
}

func TestHostMuxRejectsUnknownHostAndDispatchesKnownHost(t *testing.T) {
	registry, _ := New(fixtureApps())
	healthRouter := gin.New()
	healthRouter.NoRoute(func(context *gin.Context) { context.Status(http.StatusNoContent) })
	journalRouter := gin.New()
	journalRouter.NoRoute(func(context *gin.Context) { context.Status(http.StatusAccepted) })
	routers := map[AppID]*gin.Engine{
		Health: healthRouter, Journal: journalRouter,
	}
	mux, err := NewHostMux(registry, routers)
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
