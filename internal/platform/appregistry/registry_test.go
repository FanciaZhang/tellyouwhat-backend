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
	routers := map[AppID]http.Handler{
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

func TestFourAppsShareOneListenerWithoutForwardedHostRouting(t *testing.T) {
	apps := append(fixtureApps(),
		App{ID: Birthday, DisplayName: "生日", Hosts: []string{"api.birthday.test"}, TeamID: "TEAM", BundleID: "birthday.bundle", ManagedAIProductID: "birthday.assistant", AllowedOperationPrefix: "birthday."},
		App{ID: Albums, DisplayName: "相册", Hosts: []string{"api.albums.test"}, TeamID: "TEAM", BundleID: "albums.bundle"})
	registry, err := New(apps)
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[AppID]http.Handler{}
	for _, app := range apps {
		handlers[app.ID] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Test-App", string(app.ID)) })
	}
	mux, err := NewHostMux(registry, handlers)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	for _, app := range apps {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/readyz", nil)
		req.Host = app.Hosts[0]
		req.Header.Set("X-Forwarded-Host", "api.albums.test")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("X-Test-App") != string(app.ID) {
			t.Fatal("cross-app dispatch")
		}
	}
	delete(handlers, Albums)
	if _, err := NewHostMux(registry, handlers); err == nil {
		t.Fatal("missing album handler accepted")
	}
	album := apps[3]
	if album.AllowsOperation("journal.organize") || album.AllowsOperation("meal_decision") {
		t.Fatal("album inherited AI access")
	}
}
