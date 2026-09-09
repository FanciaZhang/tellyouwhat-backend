package adminportal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type deliveryOffers struct {
	OfferManager
	fail   bool
	active bool
	reads  int
}

func (f *deliveryOffers) ListOffers(context.Context) ([]appstoreconnect.Offer, error) {
	f.reads++
	if f.fail {
		return nil, errors.New("fixture outage")
	}
	return []appstoreconnect.Offer{{ID: "offer-1", Name: "FRIENDS", Active: f.active}}, nil
}
func (f *deliveryOffers) ListCodePools(context.Context, string) ([]appstoreconnect.CodePool, error) {
	return []appstoreconnect.CodePool{{ID: "pool-1", Kind: "oneTime", NumberOfCodes: 2, Active: true, Environment: "PRODUCTION", ExpirationDate: "2030-01-01"}}, nil
}
func (f *deliveryOffers) DownloadOneTimeCodes(context.Context, string) ([]byte, error) {
	return []byte("Code\nABC123\nXYZ456"), nil
}

func TestDeliveryHTTPNamedWorkflowAndIsolation(t *testing.T) {
	db := testutil.MySQL(t)
	ctx := context.Background()
	now := time.Now().UTC()
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
	session := adminauth.Session{UserID: repo.user.ID, CSRFToken: "fixture-csrf", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), ReauthenticatedAt: now}
	put := func() {
		t.Helper()
		if err := sessions.PutSession(ctx, adminauth.TokenHash(token), session, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	put()
	apple := &deliveryOffers{active: true}
	server := &Server{auth: auth, now: func() time.Time { return now }, offers: map[string]OfferManager{"health": apple, "journal": apple}, config: Config{Delivery: store, PreviewSigningKey: bytes.Repeat([]byte{1}, 32)}}
	router := gin.New()
	router.Use(limitAdminRequestBody())
	adminhttpapi.RegisterHandlers(router, &adminHTTPServer{Server: server, Service: auth})
	base := "/api/v1/apps/health/offers/offer-1/code-pools/pool-1/delivery"
	call := func(method, path string, body any, authenticated, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.AddCookie(&http.Cookie{Name: "__Host-tellyouwhat_admin_session", Value: token})
		}
		if csrf {
			req.Header.Set("Origin", "https://admin.example.test")
			req.Header.Set("X-Admin-CSRF", session.CSRFToken)
		}
		r := httptest.NewRecorder()
		router.ServeHTTP(r, req)
		return r
	}
	check := func(r *httptest.ResponseRecorder, want int) {
		t.Helper()
		if r.Code != want {
			t.Fatalf("HTTP %d want %d: %s", r.Code, want, r.Body.String())
		}
	}
	command := func(in deliveryCommand) *httptest.ResponseRecorder { return call("POST", base, in, true, true) }
	check(call("GET", base, nil, false, false), 401)
	check(call("GET", "/api/v1/apps/health/offers/unknown/code-pools", nil, true, false), 404)
	check(call("POST", "/api/v1/apps/health/one-time-code-batches/unknown/download", nil, true, true), 404)

	check(call("POST", base, deliveryCommand{Action: "sync"}, true, false), 403)
	check(command(deliveryCommand{Action: "sync"}), 200)
	check(call("POST", "/api/v1/apps/health/offers/other/code-pools/pool-1/delivery", deliveryCommand{Action: "sync"}, true, true), 404)
	check(call("GET", "/api/v1/apps/health/offers/other/code-pools/pool-1/delivery", nil, true, false), 404)
	session.ReauthenticatedAt = time.Time{}
	put()
	check(command(deliveryCommand{Action: "import"}), 401)
	session.ReauthenticatedAt = now
	put()
	check(command(deliveryCommand{Action: "import"}), 200)
	in := deliveryCommand{Action: "request", RequestKey: "request-fixture-1", Recipient: offerdelivery.Recipient{Name: "测试领取人", Contact: "fixture@example.test", Channel: "朋友"}}
	r := command(in)
	check(r, 200)
	var recipient offerdelivery.Request
	if err = json.Unmarshal(r.Body.Bytes(), &recipient); err != nil {
		t.Fatal(err)
	}
	replay := command(in)
	check(replay, 200)
	var repeated offerdelivery.Request
	json.Unmarshal(replay.Body.Bytes(), &repeated)
	if recipient.ID != repeated.ID {
		t.Fatal("duplicate application")
	}
	in = deliveryCommand{Action: "assign", RequestID: recipient.ID, Version: 1}
	check(command(in), 409)
	check(command(deliveryCommand{Action: "confirm_inventory", ExpectedCount: 1}), 409)
	check(command(deliveryCommand{Action: "confirm_inventory", ExpectedCount: 2}), 200)
	check(command(in), 200)
	check(command(in), 409)
	session.ReauthenticatedAt = time.Time{}
	put()
	check(command(deliveryCommand{Action: "reveal", RequestID: recipient.ID}), 401)
	session.ReauthenticatedAt = now
	put()
	r = command(deliveryCommand{Action: "reveal", RequestID: recipient.ID})
	check(r, 200)
	if r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret can be cached")
	}
	check(command(deliveryCommand{Action: "deliver", RequestID: recipient.ID, Version: 2}), 200)
	check(command(deliveryCommand{Action: "report_redeemed", RequestID: recipient.ID, Version: 3}), 200)
	// The ledger remains readable when Apple is unavailable; new allocations do not bypass the outage.
	apple.fail = true
	r = command(deliveryCommand{Action: "search", Query: "fixture"})
	check(r, 200)
	var page struct {
		Requests []offerdelivery.Request
		Summary  offerdelivery.Summary
	}
	json.Unmarshal(r.Body.Bytes(), &page)
	if len(page.Requests) != 1 || page.Summary.Delivered != 1 || page.Summary.ReportedRedeemed != 1 || page.Summary.LinkedVerified != 0 {
		t.Fatal("delivery confused with verified redemption")
	}
	check(command(deliveryCommand{Action: "sync"}), 503)
	check(command(deliveryCommand{Action: "search", Query: "absent"}), 200)
	// App-scoped operators cannot observe another App, even with a guessed record ID.
	repo.user.Role = adminauth.RoleOperator
	repo.user.AppIDs = []string{"journal"}
	check(call("GET", base, nil, true, false), 403)
	repo.user.Role = adminauth.RoleAdmin
	p, _ := store.GetPool(ctx, "health", "pool-1")
	p.ID = "pool-2"
	if err = store.SyncPool(ctx, "health", p); err != nil {
		t.Fatal(err)
	}
	check(call("POST", "/api/v1/apps/health/offers/offer-1/code-pools/pool-2/delivery", deliveryCommand{Action: "reveal", RequestID: recipient.ID}, true, true), 404)

	apple.fail = false
	current, err := store.Get(ctx, "health", recipient.ID)
	if err != nil {
		t.Fatal(err)
	}
	shared := command(deliveryCommand{Action: "share_claim", RequestID: current.ID, Version: current.Version})
	check(shared, 200)
	var linkResponse struct {
		Token   string
		Request offerdelivery.Request
	}
	json.Unmarshal(shared.Body.Bytes(), &linkResponse)
	public := func(in claimCommand) *httptest.ResponseRecorder {
		return call("POST", "/api/v1/offer-claim", in, false, false)
	}
	check(public(claimCommand{Action: "status", Token: linkResponse.Token}), 200)
	check(public(claimCommand{Action: "apply", Token: linkResponse.Token}), 422)
	check(public(claimCommand{Action: "status", Token: linkResponse.Token + "tampered"}), 404)
	received := public(claimCommand{Action: "claim", Token: linkResponse.Token})
	check(received, 200)
	if received.Header().Get("Cache-Control") != "no-store" || !bytes.Contains(received.Body.Bytes(), []byte(`"code"`)) {
		t.Fatal("claim did not privately deliver code")
	}
	privateStatus := public(claimCommand{Action: "status", Token: linkResponse.Token})
	check(privateStatus, 200)
	if bytes.Contains(privateStatus.Body.Bytes(), []byte("fixture@example.test")) || bytes.Contains(privateStatus.Body.Bytes(), []byte(`"code"`)) || bytes.Contains(privateStatus.Body.Bytes(), []byte(`"name"`)) {
		t.Fatal("status exposed unnecessary private data")
	}
	current, err = store.Get(ctx, "health", recipient.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(command(deliveryCommand{Action: "revoke_claim", RequestID: current.ID, Version: current.Version}), 200)
	check(public(claimCommand{Action: "claim", Token: linkResponse.Token}), 404)
	current, err = store.Get(ctx, "health", recipient.ID)
	if err != nil {
		t.Fatal(err)
	}
	reshared := command(deliveryCommand{Action: "share_claim", RequestID: current.ID, Version: current.Version})
	check(reshared, 200)
	var newLink struct{ Token string }
	json.Unmarshal(reshared.Body.Bytes(), &newLink)
	if newLink.Token == linkResponse.Token {
		t.Fatal("revoked link reactivated")
	}
	check(public(claimCommand{Action: "status", Token: linkResponse.Token}), 404)
	check(public(claimCommand{Action: "status", Token: newLink.Token}), 200)
	now = now.Add(91 * 24 * time.Hour)
	check(public(claimCommand{Action: "status", Token: newLink.Token}), 404)
}
