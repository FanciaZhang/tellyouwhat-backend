package adminportal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
)

func TestUnifiedAdminListsAllAppsAndPreservesOperatorScope(t *testing.T) {
	apps := []AdminApp{{ID: "health", DisplayName: "告你健康"}, {ID: "journal", DisplayName: "告你手记"}, {ID: "birthday", DisplayName: "告你生日"}, {ID: "albums", DisplayName: "告你相册"}}
	for _, role := range []adminauth.Role{adminauth.RoleAdmin, adminauth.RoleOperator} {
		t.Run(string(role), func(t *testing.T) {
			now := time.Now().UTC()
			repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: role, Status: adminauth.UserStatusActive, AppIDs: []string{"journal", "birthday"}}}
			state := adminauth.NewMemoryStateStore(func() time.Time { return now })
			auth, err := adminauth.NewService(repo, state, adminauth.Config{RPID: "admin-test.tellyouwhat.cn", Origin: "https://admin-test.tellyouwhat.cn", AppIDs: []string{"health", "journal", "birthday", "albums"}}, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			token := uuid.NewString()
			if err := state.PutSession(context.Background(), adminauth.TokenHash(token), adminauth.Session{UserID: repo.user.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}, time.Hour); err != nil {
				t.Fatal(err)
			}
			server, err := NewServer(auth, map[string]OfferManager{"health": browserOffers{}}, NewMySQLOperationStore(nil), NewMySQLMetricsReader(nil), Config{Apps: apps, PreviewSigningKey: make([]byte, 32), Environment: "development"}, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			call := func(path string, signed bool) *httptest.ResponseRecorder {
				request := httptest.NewRequest("GET", "https://admin-test.tellyouwhat.cn"+path, nil)
				if signed {
					request.AddCookie(&http.Cookie{Name: "__Host-tellyouwhat_admin_session", Value: token})
				}
				response := httptest.NewRecorder()
				server.Router().ServeHTTP(response, request)
				return response
			}
			if call("/api/v1/apps", false).Code != 401 {
				t.Fatal("anonymous App registry exposed")
			}
			response := call("/api/v1/apps", true)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			var data struct {
				Apps []AdminApp `json:"apps"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			expected := 4
			if role == adminauth.RoleOperator {
				expected = 2
			}
			if len(data.Apps) != expected {
				t.Fatal("wrong App scope", data.Apps)
			}
			for _, app := range data.Apps {
				if !repo.user.CanAccessApp(app.ID) {
					t.Fatal("unassigned App exposed", app.ID)
				}
			}
			if role == adminauth.RoleOperator && call("/api/v1/apps/health/offers", true).Code != 403 {
				t.Fatal("cross-App request was not denied")
			}
			if call("/api/v1/apps/journal/offers", true).Code != 503 {
				t.Fatal("missing optional service was not explicit")
			}
		})
	}
}
