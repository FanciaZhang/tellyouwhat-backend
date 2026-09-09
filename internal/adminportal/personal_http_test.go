package adminportal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/adminauth"
	"github.com/tellyouwhat/backend/internal/adminhttpapi"
	"github.com/tellyouwhat/backend/internal/appstoreconnect"
	"github.com/tellyouwhat/backend/internal/offerdelivery"
)

type personalHTTPOffers struct {
	OfferManager
	calls int
	pool  appstoreconnect.CodePool
}

func (f *personalHTTPOffers) OfferScope() (string, string) { return "123", "456" }
func (f *personalHTTPOffers) ListOffers(context.Context) ([]appstoreconnect.Offer, error) {
	return []appstoreconnect.Offer{{ID: "friends", Name: "FRIENDS", Active: true, SubscriptionID: "456", ProductID: "app.monthly"}}, nil
}
func (f *personalHTTPOffers) ListCodePools(context.Context, string) ([]appstoreconnect.CodePool, error) {
	return []appstoreconnect.CodePool{f.pool}, nil
}
func (f *personalHTTPOffers) CreateCustomCode(_ context.Context, _ string, code string, count int, expiry string) (appstoreconnect.CodePool, error) {
	f.calls++
	f.pool = appstoreconnect.CodePool{ID: "private-pool", Code: code, Kind: "custom", NumberOfCodes: count, ExpirationDate: expiry, Active: true}
	return f.pool, nil
}
func TestPersonalHTTPRejectsUnsupportedSingleRedemptionWithoutCloudWrite(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := &offerdelivery.Store{} // Any database access in the unsupported flow must fail the test.
	repo := &aiAuthFixture{user: adminauth.User{ID: uuid.NewString(), Role: adminauth.RoleAdmin, Status: adminauth.UserStatusActive}}
	sessions := adminauth.NewMemoryStateStore(func() time.Time { return now })
	auth, err := adminauth.NewService(repo, sessions, adminauth.Config{RPID: "admin.example.test", Origin: "https://admin.example.test", AppIDs: []string{"health", "journal"}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	session := adminauth.Session{UserID: repo.user.ID, CSRFToken: "csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ReauthenticatedAt: now}
	if err = sessions.PutSession(ctx, adminauth.TokenHash(token), session, time.Hour); err != nil {
		t.Fatal(err)
	}
	apple := &personalHTTPOffers{}
	server := &Server{auth: auth, now: func() time.Time { return now }, offers: map[string]OfferManager{"health": apple, "journal": apple}, config: Config{Delivery: store, WritesEnabled: true, PreviewSigningKey: bytes.Repeat([]byte{1}, 32)}}
	router := gin.New()
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: server, Service: auth})
	call := func(method, path string, body any, cookie, csrf bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://admin.example.test")
		if cookie {
			request.AddCookie(&http.Cookie{Name: "__Host-tellyouwhat_admin_session", Value: token})
		}
		if csrf {
			request.Header.Set("X-Admin-CSRF", "csrf")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		return w
	}
	path := "/api/v1/apps/health/offers/friends/personal-deliveries"
	id := uuid.NewString()
	body := map[string]any{"action": "create", "requestKey": id, "name": "Private friend", "expiration": now.AddDate(0, 0, 30).Format("2006-01-02")}
	if got := call("POST", path, body, false, true); got.Code != 401 {
		t.Fatalf("unauthenticated %d", got.Code)
	}
	if got := call("POST", path, body, true, false); got.Code != 403 {
		t.Fatalf("missing csrf %d", got.Code)
	}
	server.config.WritesEnabled = false
	if got := call("POST", path, body, true, true); got.Code != 403 {
		t.Fatalf("writes disabled %d", got.Code)
	}
	server.config.WritesEnabled = true
	if apple.calls != 0 {
		t.Fatal("unauthorized cloud write")
	}

	for _, action := range []string{"create", "resume"} {
		body["action"] = action
		w := call("POST", path, body, true, true)
		if w.Code != 410 || !bytes.Contains(w.Body.Bytes(), []byte("personal_code_unsupported")) {
			t.Fatalf("unsupported single redemption: %d %s", w.Code, w.Body.String())
		}
	}
	if apple.calls != 0 {
		t.Fatal("unsupported flow contacted Apple")
	}
}
