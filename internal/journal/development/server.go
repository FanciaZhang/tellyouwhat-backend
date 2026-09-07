// Package development is used only by cmd/journaldevserver. The production
// gateway must never import it. Access is through a private SSH tunnel.
package development

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/gateway"
	"github.com/tellyouwhat/backend/internal/journal/provider"
	"github.com/tellyouwhat/backend/internal/journal/service"
	"github.com/tellyouwhat/backend/internal/journal/voice"
	"github.com/tellyouwhat/backend/internal/media"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
	"github.com/tellyouwhat/backend/internal/quota"
	"github.com/tellyouwhat/backend/internal/usage"
)

type Config struct {
	Token     string
	ExpiresAt time.Time
	Organizer provider.Organizer
	Speech    voice.Speech
	Rewriter  voice.Rewriter
	Now       func() time.Time
}

type principalContextKey struct{}
type authenticator struct{}

func (authenticator) Authenticate(ctx context.Context, _ attestation.RequestProof) (attestation.Principal, error) {
	p, ok := ctx.Value(principalContextKey{}).(attestation.Principal)
	if !ok {
		return p, attestation.ErrAuthentication
	}
	return p, nil
}

// All state belongs to this developer session, not to an installation or an
// Apple purchase. No production database, Redis, or object storage is opened.
func New(c Config) (http.Handler, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	now := c.Now()
	if len(c.Token) < 43 || !c.ExpiresAt.After(now) || c.ExpiresAt.Sub(now) > 24*time.Hour || c.Organizer == nil || c.Speech == nil || c.Rewriter == nil {
		return nil, errors.New("developer session requires a random token, providers and an expiry within 24 hours")
	}
	digest := sha256.Sum256([]byte(c.Token))
	id := hex.EncodeToString(digest[:])
	p := attestation.Principal{AppID: "journal", KeyID: id, DeviceID: id, TransactionID: "development:" + id}
	store := entitlement.NewMemoryStore()
	if err := store.Upsert(context.Background(), entitlement.Record{KeyID: id, TransactionID: p.TransactionID, Environment: "development", StartedAt: now, ExpiresAt: c.ExpiresAt}); err != nil {
		return nil, err
	}
	limiter := quota.NewMemoryLimiter(quota.Limits{DailyTokensPerTransaction: 100_000, MonthlyTokensPerTransaction: 100_000, RequestsPerMinutePerOperation: 10, MaxConcurrentPerDevice: 2})
	consent := privacy.NewService(privacy.NewMemoryRepository(), nil, nil, c.Now)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	guard := func(g *gin.Context) {
		w, r := g.Writer, g.Request
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Journal-Environment", "development")
		if !c.Now().Before(c.ExpiresAt) {
			g.Abort()
			deny(w, 401, "development_session_expired")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
			g.Abort()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ready","environment":"development"}`))
			return
		}
		// The stream uses an expiring, single-use ticket issued by the authenticated
		// session route. Never substitute the developer credential for that ticket.
		stream := r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/journal/voice/sessions/") && strings.HasSuffix(r.URL.Path, "/stream")
		allowed := stream || (r.Method == http.MethodGet && r.URL.Path == "/v1/ai/quota") ||
			(r.Method == http.MethodPost && (r.URL.Path == "/v1/privacy/consents" || r.URL.Path == "/v1/journal/voice/sessions" || r.URL.Path == "/v1/ai/operations/journal.organize/responses"))
		if !allowed {
			g.Abort()
			deny(w, 404, "not_found")
			return
		}
		if !stream {
			provided := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(provided[:], digest[:]) != 1 {
				g.Abort()
				deny(w, 401, "development_access_denied")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
		}
		g.Request = r
		g.Next()
	}
	s := gateway.New(gateway.Dependencies{
		HTTPMiddleware: []gin.HandlerFunc{guard},
		App:            appregistry.App{ID: appregistry.Journal, AllowedOperationPrefix: "journal."},
		Authenticator:  authenticator{}, Entitlements: entitlement.NewChecker(store, c.Now),
		Quota: limiter, QuotaReader: limiter, Usage: usage.NewMemoryRecorder(),
		Media:   media.NewService(nil, media.NewMemoryRegistry(), c.Now),
		Privacy: consent, Consent: consent,
		RequiredConsentScopes: []string{privacy.ManagedAIScope}, AllowedConsentScopes: []string{privacy.ManagedAIScope},
		JournalOrganizer:       &service.Organizer{Model: c.Organizer, LiteMaxCharacters: 6000, LiteMaxBooks: 24, LiteMaxTags: 80, AnalysisVersion: "journal-organize-2026-08-31"},
		JournalAnalysisVersion: "journal-organize-2026-08-31",
		Voice:                  &voice.Service{Store: voice.NewMemoryStore(), Speech: c.Speech, Model: c.Rewriter, Secret: secret, Limit: 10 * 60 * 1000}, VoiceEntitlements: store,
		Now: c.Now,
	})
	router := s.Router()
	router.ContextWithFallback = true
	return router, nil
}

func deny(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"developer service request denied","requestID":""}}`))
}
