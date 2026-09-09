package adminportal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/tellyouwhat/backend/internal/storage/mysqlstore"
	"github.com/tellyouwhat/backend/internal/testutil"
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
func TestPersonalHTTPExistingInventoryAuthClaimAndRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	db := testutil.MySQL(t)
	cipher, err := mysqlstore.NewPayloadCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	store := &offerdelivery.Store{DB: db, Cipher: cipher}
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
	expiry := now.AddDate(0, 0, 30).Format("2006-01-02")
	apple := &personalHTTPOffers{pool: appstoreconnect.CodePool{ID: "batch-500", Kind: "oneTime", Environment: "PRODUCTION", NumberOfCodes: 500, ExpirationDate: expiry, Active: true}}
	end, _ := offerdelivery.AppleCodeExpiry(expiry)
	p := offerdelivery.Pool{ID: "batch-500", OfferID: "friends", OfferName: "FRIENDS", SubscriptionID: "456", ProductID: "app.monthly", Kind: "oneTime", Environment: "production", Capacity: 500, Active: true, ExpiresAt: end, SyncedAt: now}
	if err = store.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	codes := []string{}
	for i := 0; i < 500; i++ {
		codes = append(codes, fmt.Sprintf("TEST%06d", i))
	}
	if err = store.ImportCodes(ctx, "health", p.ID, "admin", codes, now); err != nil {
		t.Fatal(err)
	}
	if err = store.ConfirmExternalInventory(ctx, "health", p.ID, "admin", 500, now); err != nil {
		t.Fatal(err)
	}
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
	body := map[string]any{"action": "create", "requestKey": id, "name": "Private friend", "poolID": p.ID}
	if got := call("POST", path, body, false, true); got.Code != 401 {
		t.Fatalf("unauthenticated %d", got.Code)
	}
	if got := call("POST", path, body, true, false); got.Code != 403 {
		t.Fatalf("missing csrf %d", got.Code)
	}

	// Allocating existing stock must not require permission to create new batches at Apple.
	server.config.WritesEnabled = false
	w := call("POST", path, body, true, true)
	if w.Code != 200 {
		t.Fatalf("allocation %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Delivery offerdelivery.PersonalDelivery
		Request  offerdelivery.Request
		Token    string
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Token == "" || result.Request.CodeID == "" {
		t.Fatal("missing claim", err)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("TEST000")) {
		t.Fatal("creation exposed code")
	}
	if got := call("POST", path, body, true, true); got.Code != 200 {
		t.Fatal("retry failed", got.Code)
	}
	summary, err := store.Summary(ctx, "health", p.ID)
	if err != nil || summary.Available != 499 || summary.AssignedCodes != 1 || summary.Applications != 0 {
		t.Fatalf("incorrect stock %+v %v", summary, err)
	}
	claimed := call("POST", "/api/v1/offer-claim", map[string]string{"action": "claim", "token": result.Token}, false, false)
	if claimed.Code != 200 || !bytes.Contains(claimed.Body.Bytes(), []byte("https://apps.apple.com/redeem?")) {
		t.Fatalf("claim %d %s", claimed.Code, claimed.Body.String())
	}
	r, err := store.Get(ctx, "health", result.Request.ID)
	if err != nil || r.ClaimedAt == nil || r.VerifiedAt != nil {
		t.Fatal("claim/redemption separation", err)
	}
	list := call("GET", path, nil, true, false)
	if list.Code != 200 || !bytes.Contains(list.Body.Bytes(), []byte("Private friend")) || bytes.Contains(list.Body.Bytes(), []byte("appleReport")) {
		t.Fatalf("individual status %d %s", list.Code, list.Body.String())
	}
	r, err = store.SetClaimLink(ctx, "health", r.ID, "admin", r.Version, false, now)
	if err != nil {
		t.Fatal(err)
	}
	replay := call("POST", path, map[string]string{"action": "resume", "requestKey": id}, true, true)
	if replay.Code != 200 || bytes.Contains(replay.Body.Bytes(), []byte(`"token"`)) {
		t.Fatal("revoked link revived")
	}
	cross := call("POST", "/api/v1/apps/journal/offers/friends/personal-deliveries", map[string]string{"action": "resume", "requestKey": id}, true, true)
	if cross.Code != 404 {
		t.Fatal("cross-App record", cross.Code)
	}
	if apple.calls != 0 {
		t.Fatal("allocated by creating codes at Apple")
	}
}
